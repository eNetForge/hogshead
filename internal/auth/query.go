package auth

import (
	"net/url"
	"slices"
	"strings"
)

// queryPair is one decoded key/value pair of a query string.
type queryPair struct{ key, value string }

// parseRawQuery splits a raw query string on '&' and '=' and percent-decodes
// each side ('+' means space). Unlike url.ParseQuery it never drops pairs
// (for example ones containing ';', which appear in X-Amz-SignedHeaders),
// and undecodable components are kept verbatim.
func parseRawQuery(raw string) []queryPair {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "&")
	pairs := make([]queryPair, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		pairs = append(pairs, queryPair{key: queryDecode(k), value: queryDecode(v)})
	}
	return pairs
}

func queryDecode(s string) string {
	d, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}
	return d
}

// queryLookup returns the first value for name (case-insensitive).
func queryLookup(pairs []queryPair, name string) (string, bool) {
	for _, p := range pairs {
		if strings.EqualFold(p.key, name) {
			return p.value, true
		}
	}
	return "", false
}

// canonicalQuery builds the SigV4 canonical query string: each key and value
// AWS URI-encoded, sorted by (encoded) key then value, joined with '&'. The
// parameter named exclude (case-insensitive) is omitted.
func canonicalQuery(raw, exclude string) string {
	pairs := parseRawQuery(raw)
	encoded := make([]queryPair, 0, len(pairs))
	for _, p := range pairs {
		if exclude != "" && strings.EqualFold(p.key, exclude) {
			continue
		}
		encoded = append(encoded, queryPair{key: uriEncode(p.key, true), value: uriEncode(p.value, true)})
	}
	slices.SortFunc(encoded, func(a, b queryPair) int {
		if c := strings.Compare(a.key, b.key); c != 0 {
			return c
		}
		return strings.Compare(a.value, b.value)
	})
	return joinPairs(encoded)
}

// encodeQuery AWS URI-encodes decoded pairs and joins them as key=value&...
// in the order given (no sorting).
func encodeQuery(pairs []queryPair) string {
	encoded := make([]queryPair, len(pairs))
	for i, p := range pairs {
		encoded[i] = queryPair{key: uriEncode(p.key, true), value: uriEncode(p.value, true)}
	}
	return joinPairs(encoded)
}

// joinPairs joins already-encoded pairs with '=' and '&'.
func joinPairs(pairs []queryPair) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.key)
		b.WriteByte('=')
		b.WriteString(p.value)
	}
	return b.String()
}
