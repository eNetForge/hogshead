package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/meta"
)

func parseCORS(raw string) *s3xml.CORSConfiguration {
	if raw == "" {
		return nil
	}
	var cfg s3xml.CORSConfiguration
	if err := s3xml.Unmarshal(strings.NewReader(raw), &cfg, s3xml.DefaultLimit); err != nil {
		return nil
	}
	return &cfg
}

func originMatches(pattern, origin string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, origin)
	}
	i := strings.Index(pattern, "*")
	pre, post := pattern[:i], pattern[i+1:]
	return len(origin) >= len(pre)+len(post) && strings.HasPrefix(strings.ToLower(origin), strings.ToLower(pre)) && strings.HasSuffix(strings.ToLower(origin), strings.ToLower(post))
}

func matchCORSRule(cfg *s3xml.CORSConfiguration, origin, method string, reqHeaders []string) *s3xml.CORSRule {
	if cfg == nil || origin == "" {
		return nil
	}
	for i := range cfg.CORSRule {
		rule := &cfg.CORSRule[i]
		okOrigin := false
		for _, o := range rule.AllowedOrigin {
			if originMatches(o, origin) {
				okOrigin = true
				break
			}
		}
		if !okOrigin {
			continue
		}
		okMethod := false
		for _, m := range rule.AllowedMethod {
			if strings.EqualFold(m, method) {
				okMethod = true
				break
			}
		}
		if !okMethod {
			continue
		}
		okHeaders := true
		for _, rh := range reqHeaders {
			found := false
			for _, ah := range rule.AllowedHeader {
				if ah == "*" || strings.EqualFold(ah, rh) || (strings.HasSuffix(ah, "*") && strings.HasPrefix(strings.ToLower(rh), strings.ToLower(strings.TrimSuffix(ah, "*")))) {
					found = true
					break
				}
			}
			if !found {
				okHeaders = false
				break
			}
		}
		if okHeaders {
			return rule
		}
	}
	return nil
}

func setCORSHeaders(h http.Header, rule *s3xml.CORSRule, origin string, preflight bool, reqHeaders []string) {
	allowOrigin := origin
	for _, o := range rule.AllowedOrigin {
		if o == "*" {
			allowOrigin = "*"
		}
	}
	h.Set("Access-Control-Allow-Origin", allowOrigin)
	if allowOrigin != "*" {
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Add("Vary", "Origin")
	}
	if len(rule.ExposeHeader) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeader, ", "))
	}
	if preflight {
		h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethod, ", "))
		if len(reqHeaders) > 0 {
			h.Set("Access-Control-Allow-Headers", strings.Join(reqHeaders, ", "))
		}
		if rule.MaxAgeSeconds != nil {
			h.Set("Access-Control-Max-Age", strconv.Itoa(*rule.MaxAgeSeconds))
		}
	}
}

func requestHeaderList(r *http.Request) []string {
	var out []string
	for _, part := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyCORS adds CORS headers to a normal (non-preflight) response.
func (h *Handler) applyCORS(c *reqCtx) {
	origin := c.r.Header.Get("Origin")
	if origin == "" || c.bucket == nil {
		return
	}
	rule := matchCORSRule(parseCORS(c.bucket.CORS), origin, c.r.Method, nil)
	if rule != nil {
		setCORSHeaders(c.w.Header(), rule, origin, false, nil)
	}
}

// preflight answers OPTIONS requests from browsers.
func (h *Handler) preflight(c *reqCtx) error {
	origin := c.r.Header.Get("Origin")
	method := c.r.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" {
		return s3err.New(s3err.AccessDenied).WithMessage("Insufficient information. Origin request header needed.")
	}
	if c.target.Bucket == "" {
		return s3err.New(s3err.AccessDenied)
	}
	b, err := h.d.DB.GetBucket(c.ctx(), c.target.Bucket)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchBucket) || b == nil {
			return s3err.New(s3err.AccessDenied).WithMessage("CORSResponse: Bucket not found")
		}
		return err
	}
	reqHeaders := requestHeaderList(c.r)
	rule := matchCORSRule(parseCORS(b.CORS), origin, method, reqHeaders)
	if rule == nil {
		return s3err.New(s3err.AccessDenied).WithMessage("CORSResponse: This CORS request is not allowed.")
	}
	setCORSHeaders(c.w.Header(), rule, origin, true, reqHeaders)
	return c.empty(http.StatusOK)
}

var allowedCORSMethods = map[string]bool{"GET": true, "PUT": true, "HEAD": true, "POST": true, "DELETE": true}

func ValidateCORS(cfg *s3xml.CORSConfiguration) error {
	if len(cfg.CORSRule) == 0 {
		return s3err.New(s3err.MalformedXML).WithMessage("At least one CORSRule is required")
	}
	if len(cfg.CORSRule) > 100 {
		return s3err.New(s3err.InvalidRequest).WithMessage("At most 100 CORS rules are allowed")
	}
	for _, r := range cfg.CORSRule {
		if len(r.AllowedOrigin) == 0 || len(r.AllowedMethod) == 0 {
			return s3err.New(s3err.MalformedXML).WithMessage("Each CORSRule needs AllowedOrigin and AllowedMethod")
		}
		for _, m := range r.AllowedMethod {
			if !allowedCORSMethods[m] {
				return s3err.Newf(s3err.InvalidRequest, "Found unsupported HTTP method in CORS config. Unsupported method is %s", m)
			}
		}
		for _, o := range r.AllowedOrigin {
			if strings.Count(o, "*") > 1 {
				return s3err.New(s3err.InvalidRequest).WithMessage("AllowedOrigin can contain at most one wildcard")
			}
		}
	}
	return nil
}
