// Package policy implements the supported subset of S3 bucket policies and
// the role-based authorization model that sits in front of them.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Policy is a parsed bucket policy.
type Policy struct {
	Version    string      `json:"Version,omitempty"`
	ID         string      `json:"Id,omitempty"`
	Statements []Statement `json:"Statement"`
}

// Statement is one policy statement.
type Statement struct {
	Sid       string    `json:"Sid,omitempty"`
	Effect    string    `json:"Effect"`
	Principal Principal `json:"Principal"`
	Action    StrList   `json:"Action"`
	Resource  StrList   `json:"Resource"`
	// Condition maps operator -> key -> values. See supportedOperators.
	Condition map[string]map[string]StrList `json:"Condition,omitempty"`
}

// supportedOperators lists the condition operators we evaluate.
var supportedOperators = map[string]bool{
	"StringEquals": true, "StringNotEquals": true, "StringEqualsIgnoreCase": true, "StringNotEqualsIgnoreCase": true,
	"StringLike": true, "StringNotLike": true, "IpAddress": true, "NotIpAddress": true, "Bool": true,
}

// Principal is "*" or {"AWS": "*" | [ids]}.
type Principal struct {
	All bool
	AWS []string
}

// UnmarshalJSON accepts both forms.
func (p *Principal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s != "*" {
			return fmt.Errorf("principal %q: only \"*\" or {\"AWS\": ...} is supported", s)
		}
		p.All = true
		return nil
	}
	var obj struct {
		AWS StrList `json:"AWS"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("principal: %w", err)
	}
	if len(obj.AWS) == 0 {
		return errors.New("principal: AWS list is empty")
	}
	for _, a := range obj.AWS {
		if a == "*" {
			p.All = true
			continue
		}
		p.AWS = append(p.AWS, principalName(a))
	}
	return nil
}

// MarshalJSON renders the canonical form.
func (p Principal) MarshalJSON() ([]byte, error) {
	if p.All && len(p.AWS) == 0 {
		return []byte(`"*"`), nil
	}
	aws := p.AWS
	if p.All {
		aws = append([]string{"*"}, aws...)
	}
	return json.Marshal(map[string]any{"AWS": aws})
}

// principalName strips an IAM ARN down to the user name we match against.
func principalName(a string) string {
	if i := strings.LastIndex(a, "/"); i >= 0 && strings.HasPrefix(a, "arn:") {
		return a[i+1:]
	}
	return a
}

// StrList is a JSON string or list of strings.
type StrList []string

// UnmarshalJSON accepts a scalar or an array.
func (l *StrList) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*l = StrList{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*l = arr
	return nil
}

// MaxSize is the maximum accepted policy document size.
const MaxSize = 20 * 1024

// Parse validates a policy document for bucket. It rejects anything outside
// the supported subset with a descriptive error.
func Parse(doc []byte, bucket string) (*Policy, error) {
	if len(doc) > MaxSize {
		return nil, errors.New("policy document too large")
	}
	var p Policy
	dec := json.NewDecoder(strings.NewReader(string(doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid policy JSON: %w", err)
	}
	if p.Version != "" && p.Version != "2012-10-17" && p.Version != "2008-10-17" {
		return nil, fmt.Errorf("unsupported policy version %q", p.Version)
	}
	if len(p.Statements) == 0 {
		return nil, errors.New("policy has no statements")
	}
	for i, s := range p.Statements {
		if s.Effect != "Allow" && s.Effect != "Deny" {
			return nil, fmt.Errorf("statement %d: Effect must be Allow or Deny", i)
		}
		for op := range s.Condition {
			base := strings.TrimSuffix(op, "IfExists")
			if !supportedOperators[base] {
				return nil, fmt.Errorf("statement %d: condition operator %q is not supported", i, op)
			}
		}
		if len(s.Action) == 0 {
			return nil, fmt.Errorf("statement %d: Action is required", i)
		}
		for _, a := range s.Action {
			if a != "*" && !strings.HasPrefix(strings.ToLower(a), "s3:") {
				return nil, fmt.Errorf("statement %d: action %q must start with the s3 prefix", i, a)
			}
		}
		if len(s.Resource) == 0 {
			return nil, fmt.Errorf("statement %d: Resource is required", i)
		}
		for _, r := range s.Resource {
			b, _, ok := splitResource(r)
			if !ok {
				return nil, fmt.Errorf("statement %d: resource %q must be arn:aws:s3:::%s or arn:aws:s3:::%s/<key-pattern>", i, r, bucket, bucket)
			}
			if b != bucket && b != "*" {
				return nil, fmt.Errorf("statement %d: resource %q does not belong to bucket %q", i, r, bucket)
			}
		}
		if !s.Principal.All && len(s.Principal.AWS) == 0 {
			return nil, fmt.Errorf("statement %d: Principal is required", i)
		}
	}
	return &p, nil
}

// splitResource parses arn:aws:s3:::bucket[/key].
func splitResource(r string) (bucket, key string, ok bool) {
	const prefix = "arn:aws:s3:::"
	if !strings.HasPrefix(r, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(r, prefix)
	if rest == "" {
		return "", "", false
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i], rest[i+1:], true
	}
	return rest, "", true
}

// Request is what gets authorized.
type Request struct {
	// Action is an S3 action such as "s3:GetObject".
	Action string
	Bucket string
	Key    string // empty for bucket-level actions
	// Principal identifies the caller: user name, or "" for anonymous.
	Principal string
	// PrincipalID is an alternative identifier (user id) matched as well.
	PrincipalID string
	// Context holds condition keys such as aws:SourceIp or s3:prefix.
	Context map[string]string
}

// Effect is the outcome of evaluating a policy.
type Effect int

// Policy outcomes.
const (
	NoMatch Effect = iota
	Allow
	Deny
)

// Evaluate returns Deny if any matching statement denies, Allow if any
// matching statement allows, NoMatch otherwise.
func (p *Policy) Evaluate(r Request) Effect {
	if p == nil {
		return NoMatch
	}
	out := NoMatch
	for _, s := range p.Statements {
		if !s.matches(r) {
			continue
		}
		if s.Effect == "Deny" {
			return Deny
		}
		out = Allow
	}
	return out
}

func (s *Statement) matches(r Request) bool {
	if !s.Principal.All {
		found := false
		for _, a := range s.Principal.AWS {
			if (r.Principal != "" && a == r.Principal) || (r.PrincipalID != "" && a == r.PrincipalID) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !matchAny(s.Action, r.Action, true) {
		return false
	}
	resourceOK := false
	for _, res := range s.Resource {
		b, k, ok := splitResource(res)
		if !ok {
			continue
		}
		if !glob(b, r.Bucket, false) {
			continue
		}
		if r.Key == "" {
			// Bucket-level actions match only the bucket ARN, as on AWS.
			if k == "" {
				resourceOK = true
				break
			}
			continue
		}
		if k != "" && glob(k, r.Key, false) {
			resourceOK = true
			break
		}
	}
	if !resourceOK {
		return false
	}
	return conditionsMatch(s.Condition, r.Context)
}

func matchAny(patterns []string, v string, fold bool) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if glob(p, v, fold) {
			return true
		}
	}
	return false
}

// glob matches with * (any run, including '/') and ? (one character).
func glob(pattern, v string, fold bool) bool {
	if fold {
		pattern, v = strings.ToLower(pattern), strings.ToLower(v)
	}
	return globMatch(pattern, v)
}

func globMatch(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if p == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(p, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if s == "" || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return s == ""
}

// conditionsMatch evaluates every operator block; all must pass.
func conditionsMatch(cond map[string]map[string]StrList, ctx map[string]string) bool {
	for op, keys := range cond {
		ifExists := strings.HasSuffix(op, "IfExists")
		base := strings.TrimSuffix(op, "IfExists")
		for key, values := range keys {
			actual, present := lookupKey(ctx, key)
			if !present {
				if ifExists {
					continue
				}
				return false
			}
			if !evalOperator(base, actual, values) {
				return false
			}
		}
	}
	return true
}

func lookupKey(ctx map[string]string, key string) (string, bool) {
	for k, v := range ctx {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func evalOperator(op, actual string, values []string) bool {
	anyMatch := func(fn func(v string) bool) bool {
		for _, v := range values {
			if fn(v) {
				return true
			}
		}
		return false
	}
	switch op {
	case "StringEquals":
		return anyMatch(func(v string) bool { return v == actual })
	case "StringNotEquals":
		return !anyMatch(func(v string) bool { return v == actual })
	case "StringEqualsIgnoreCase":
		return anyMatch(func(v string) bool { return strings.EqualFold(v, actual) })
	case "StringNotEqualsIgnoreCase":
		return !anyMatch(func(v string) bool { return strings.EqualFold(v, actual) })
	case "StringLike":
		return anyMatch(func(v string) bool { return globMatch(v, actual) })
	case "StringNotLike":
		return !anyMatch(func(v string) bool { return globMatch(v, actual) })
	case "IpAddress":
		return anyMatch(func(v string) bool { return ipInRange(actual, v) })
	case "NotIpAddress":
		return !anyMatch(func(v string) bool { return ipInRange(actual, v) })
	case "Bool":
		return anyMatch(func(v string) bool { return strings.EqualFold(v, actual) })
	}
	return false
}

func ipInRange(ipStr, cidr string) bool {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	if !strings.Contains(cidr, "/") {
		other := net.ParseIP(cidr)
		return other != nil && other.Equal(ip)
	}
	_, n, err := net.ParseCIDR(cidr)
	return err == nil && n.Contains(ip)
}

// PublicReadPolicy returns the canonical "anyone can read and list" policy,
// equivalent to the AWS public-read canned ACL.
func PublicReadPolicy(bucket string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"PublicRead","Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:GetObjectVersion"],"Resource":["arn:aws:s3:::%s/*"]},{"Sid":"PublicList","Effect":"Allow","Principal":"*","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::%s"]}]}`, bucket, bucket)
}

// PublicReadWritePolicy is the public-read-write canned ACL equivalent.
func PublicReadWritePolicy(bucket string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"PublicRead","Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:GetObjectVersion"],"Resource":["arn:aws:s3:::%s/*"]},{"Sid":"PublicList","Effect":"Allow","Principal":"*","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::%s"]},{"Sid":"PublicWrite","Effect":"Allow","Principal":"*","Action":["s3:PutObject","s3:DeleteObject","s3:AbortMultipartUpload"],"Resource":["arn:aws:s3:::%s/*"]}]}`, bucket, bucket, bucket)
}

// IsPublicWrite reports whether anonymous PutObject is allowed on all keys.
func (p *Policy) IsPublicWrite(bucket string) bool {
	return p != nil && p.Evaluate(Request{Action: "s3:PutObject", Bucket: bucket, Key: "any/key"}) == Allow
}

// IsCannedPublic reports whether the policy is exactly one of the canned
// public policies, so a PutBucketAcl private can safely remove it.
func IsCannedPublic(doc, bucket string) bool {
	return doc == PublicReadPolicy(bucket) || doc == PublicReadWritePolicy(bucket)
}

// IsPublicRead reports whether the policy grants anonymous GetObject on all keys.
func (p *Policy) IsPublicRead(bucket string) bool {
	return p != nil && p.Evaluate(Request{Action: "s3:GetObject", Bucket: bucket, Key: "any/key"}) == Allow
}
