package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/enetforge/hogshead/internal/meta"
)

func TestParseAndEvaluate(t *testing.T) {
	doc := `{
	  "Version": "2012-10-17",
	  "Statement": [
	    {"Sid": "pub", "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::photos/public/*"},
	    {"Effect": "Allow", "Principal": {"AWS": ["arn:aws:iam:::user/alice", "bob"]}, "Action": ["s3:*"], "Resource": ["arn:aws:s3:::photos", "arn:aws:s3:::photos/*"]},
	    {"Effect": "Deny", "Principal": {"AWS": "*"}, "Action": "s3:DeleteObject", "Resource": "arn:aws:s3:::photos/keep/*"}
	  ]}`
	p, err := Parse([]byte(doc), "photos")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		r    Request
		want Effect
	}{
		{Request{Action: "s3:GetObject", Bucket: "photos", Key: "public/a.jpg"}, Allow},
		{Request{Action: "s3:GetObject", Bucket: "photos", Key: "private/a.jpg"}, NoMatch},
		{Request{Action: "s3:getobject", Bucket: "photos", Key: "public/a.jpg"}, Allow},
		{Request{Action: "s3:PutObject", Bucket: "photos", Key: "x", Principal: "alice"}, Allow},
		{Request{Action: "s3:ListBucket", Bucket: "photos", Principal: "bob"}, Allow},
		{Request{Action: "s3:ListBucket", Bucket: "photos", Principal: "carol"}, NoMatch},
		{Request{Action: "s3:DeleteObject", Bucket: "photos", Key: "keep/x", Principal: "alice"}, Deny},
		{Request{Action: "s3:DeleteObject", Bucket: "photos", Key: "other/x", Principal: "alice"}, Allow},
		{Request{Action: "s3:GetObject", Bucket: "other", Key: "public/a.jpg"}, NoMatch},
	}
	for _, c := range cases {
		if got := p.Evaluate(c.r); got != c.want {
			t.Errorf("%+v: got %v want %v", c.r, got, c.want)
		}
	}
	if p.IsPublicRead("photos") {
		t.Fatal("only a prefix is public")
	}
	pub, _ := Parse([]byte(PublicReadPolicy("photos")), "photos")
	if !pub.IsPublicRead("photos") {
		t.Fatal("public read policy")
	}
	var nilp *Policy
	if nilp.Evaluate(Request{}) != NoMatch || nilp.IsPublicRead("x") {
		t.Fatal("nil policy")
	}
	// Round trip through JSON keeps principal forms.
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"Principal":"*"`) || !strings.Contains(string(b), `"AWS":["alice","bob"]`) {
		t.Fatalf("marshal: %s", b)
	}
	b, _ = json.Marshal(Principal{All: true, AWS: []string{"x"}})
	if string(b) != `{"AWS":["*","x"]}` {
		t.Fatalf("marshal mixed principal: %s", b)
	}
}

func TestParseRejects(t *testing.T) {
	bad := map[string]string{
		"not json":        `{`,
		"no statements":   `{"Statement":[]}`,
		"bad effect":      `{"Statement":[{"Effect":"Maybe","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
		"bad condition":   `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"DateGreaterThan":{"aws:CurrentTime":"2020-01-01T00:00:00Z"}}}]}`,
		"no action":       `{"Statement":[{"Effect":"Allow","Principal":"*","Resource":"arn:aws:s3:::b/*"}]}`,
		"non s3 action":   `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"iam:*","Resource":"arn:aws:s3:::b/*"}]}`,
		"no resource":     `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject"}]}`,
		"bad resource":    `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"b/*"}]}`,
		"other bucket":    `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}]}`,
		"bad principal":   `{"Statement":[{"Effect":"Allow","Principal":"alice","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
		"empty principal": `{"Statement":[{"Effect":"Allow","Principal":{"AWS":[]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
		"service princ":   `{"Statement":[{"Effect":"Allow","Principal":{"Service":"x"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
		"version":         `{"Version":"1999","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
		"unknown field":   `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Foo":1}]}`,
		"too large":       `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/` + strings.Repeat("x", MaxSize) + `"}]}`,
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc), "b"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := Parse([]byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::*/*"}]}`), "b"); err != nil {
		t.Fatalf("wildcard bucket should parse: %v", err)
	}
}

func TestGlob(t *testing.T) {
	if !glob("a/*", "a/b/c", false) || !glob("*", "anything/at/all", false) || glob("a/?", "a/bc", false) || !glob("S3:Get*", "s3:getobject", true) {
		t.Fatal("glob")
	}
	if !glob("data[", "data[", false) || glob("a*c", "ab", false) || !glob("a**c", "abbbc", false) || glob("", "x", false) || !glob("", "", false) {
		t.Fatal("glob edge cases")
	}
}

func TestConditions(t *testing.T) {
	doc := `{"Statement":[
	 {"Effect":"Allow","Principal":"*","Action":"*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"],
	  "Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","192.168.1.7"]},"StringLike":{"s3:prefix":"public/*"},"Bool":{"aws:SecureTransport":"false"}}},
	 {"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*",
	  "Condition":{"StringNotEqualsIfExists":{"aws:username":"alice"},"StringLike":{"aws:Referer":"https://trusted.example/*"}}}
	]}`
	p, err := Parse([]byte(doc), "b")
	if err != nil {
		t.Fatal(err)
	}
	ctx := map[string]string{"aws:SourceIp": "10.1.2.3", "s3:prefix": "public/x", "aws:SecureTransport": "false"}
	if p.Evaluate(Request{Action: "s3:ListBucket", Bucket: "b", Context: ctx}) != Allow {
		t.Fatal("conditions should allow")
	}
	ctx2 := map[string]string{"aws:SourceIp": "8.8.8.8", "s3:prefix": "public/x", "aws:SecureTransport": "false"}
	if p.Evaluate(Request{Action: "s3:ListBucket", Bucket: "b", Context: ctx2}) != NoMatch {
		t.Fatal("ip outside range must not match")
	}
	ctx3 := map[string]string{"aws:SourceIp": "192.168.1.7", "s3:prefix": "private/x", "aws:SecureTransport": "false"}
	if p.Evaluate(Request{Action: "s3:ListBucket", Bucket: "b", Context: ctx3}) != NoMatch {
		t.Fatal("prefix outside pattern must not match")
	}
	if p.Evaluate(Request{Action: "s3:ListBucket", Bucket: "b", Context: map[string]string{"aws:SourceIp": "10.0.0.1"}}) != NoMatch {
		t.Fatal("missing key fails the condition")
	}
	// Deny: bob coming from a trusted referer.
	deny := map[string]string{"aws:username": "bob", "aws:Referer": "https://trusted.example/page"}
	if p.Evaluate(Request{Action: "s3:GetObject", Bucket: "b", Key: "k", Context: deny}) != Deny {
		t.Fatal("deny should match")
	}
	deny["aws:username"] = "alice"
	if p.Evaluate(Request{Action: "s3:GetObject", Bucket: "b", Key: "k", Context: deny}) == Deny {
		t.Fatal("alice is exempt")
	}
	delete(deny, "aws:username") // IfExists: missing passes
	deny["aws:Referer"] = "https://other.example/"
	if p.Evaluate(Request{Action: "s3:GetObject", Bucket: "b", Key: "k", Context: deny}) == Deny {
		t.Fatal("other referer does not match")
	}
	if evalOperator("Weird", "a", []string{"a"}) || evalOperator("NumericEquals", "5", []string{"5"}) {
		t.Fatal("unsupported operators never match")
	}
	if _, err := Parse([]byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"NumericLessThan":{"s3:max-keys":"10"}}}]}`), "b"); err == nil {
		t.Fatal("numeric operators are rejected at parse time")
	}
	if !evalOperator("StringEqualsIgnoreCase", "A", []string{"a"}) || evalOperator("StringNotEqualsIgnoreCase", "A", []string{"a"}) || evalOperator("StringNotLike", "abc", []string{"a*"}) || evalOperator("NotIpAddress", "10.0.0.1", []string{"10.0.0.0/8"}) {
		t.Fatal("string/ip operators")
	}
	if ipInRange("garbage", "10.0.0.0/8") || ipInRange("10.0.0.1", "garbage/8") {
		t.Fatal("ip parsing")
	}
	// Bucket-level actions only match the bucket ARN.
	q, _ := Parse([]byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`), "b")
	if q.Evaluate(Request{Action: "s3:DeleteBucket", Bucket: "b"}) != NoMatch || q.Evaluate(Request{Action: "s3:GetObject", Bucket: "b", Key: "k"}) != Allow {
		t.Fatal("object-only resource must not grant bucket actions")
	}
	if !q.IsPublicWrite("b") || IsCannedPublic(PublicReadWritePolicy("b"), "b") == false || IsCannedPublic("{}", "b") {
		t.Fatal("public helpers")
	}
}

func user(role string, buckets ...string) *Caller {
	return &Caller{User: &meta.User{ID: "id1", Name: "alice", Role: role, Buckets: buckets}}
}

func TestAuthorize(t *testing.T) {
	deny, _ := Parse([]byte(`{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"}]}`), "b")
	pub, _ := Parse([]byte(PublicReadPolicy("b")), "b")
	grant, _ := Parse([]byte(`{"Statement":[{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`), "b")

	cases := []struct {
		name string
		c    *Caller
		p    *Policy
		r    Request
		want bool
	}{
		{"admin anything", user(meta.RoleAdmin), deny, Request{Action: "s3:DeleteObject", Bucket: "b", Key: "k"}, true},
		{"rw object", user(meta.RoleReadWrite), nil, Request{Action: "s3:PutObject", Bucket: "b", Key: "k"}, true},
		{"rw denied by policy", user(meta.RoleReadWrite), deny, Request{Action: "s3:DeleteObject", Bucket: "b", Key: "k"}, false},
		{"rw create bucket unscoped", user(meta.RoleReadWrite), nil, Request{Action: "s3:CreateBucket", Bucket: "new"}, true},
		{"rw create bucket scoped", user(meta.RoleReadWrite, "b"), nil, Request{Action: "s3:CreateBucket", Bucket: "new"}, false},
		{"rw out of scope", user(meta.RoleReadWrite, "b"), nil, Request{Action: "s3:GetObject", Bucket: "other", Key: "k"}, false},
		{"rw scope glob", user(meta.RoleReadWrite, "team-*"), nil, Request{Action: "s3:GetObject", Bucket: "team-a", Key: "k"}, true},
		{"ro get", user(meta.RoleReadOnly), nil, Request{Action: "s3:GetObject", Bucket: "b", Key: "k"}, true},
		{"ro list", user(meta.RoleReadOnly), nil, Request{Action: "s3:ListBucket", Bucket: "b"}, true},
		{"ro put", user(meta.RoleReadOnly), nil, Request{Action: "s3:PutObject", Bucket: "b", Key: "k"}, false},
		{"ro put via policy", user(meta.RoleReadOnly), grant, Request{Action: "s3:PutObject", Bucket: "b", Key: "k"}, true},
		{"list all buckets", user(meta.RoleReadOnly, "b"), nil, Request{Action: "s3:ListAllMyBuckets"}, true},
		{"anon no policy", nil, nil, Request{Action: "s3:GetObject", Bucket: "b", Key: "k"}, false},
		{"anon public", nil, pub, Request{Action: "s3:GetObject", Bucket: "b", Key: "k"}, true},
		{"anon public list", nil, pub, Request{Action: "s3:ListBucket", Bucket: "b"}, true},
		{"anon public policy write", nil, pub, Request{Action: "s3:PutBucketPolicy", Bucket: "b"}, false},
		{"disabled user", &Caller{User: &meta.User{Name: "x", Role: meta.RoleAdmin, Disabled: true}}, nil, Request{Action: "s3:GetObject", Bucket: "b", Key: "k"}, true},
		{"rw cannot set policy", user(meta.RoleReadWrite), nil, Request{Action: "s3:PutBucketPolicy", Bucket: "b"}, false},
		{"rw cannot delete bucket", user(meta.RoleReadWrite), nil, Request{Action: "s3:DeleteBucket", Bucket: "b"}, false},
		{"disabled rw", &Caller{User: &meta.User{Name: "x", Role: meta.RoleReadWrite, Disabled: true}}, nil, Request{Action: "s3:GetObject", Bucket: "b", Key: "k"}, false},
		{"unknown role", &Caller{User: &meta.User{Name: "x", Role: "weird"}}, nil, Request{Action: "s3:GetObject", Bucket: "b", Key: "k"}, false},
	}
	for _, c := range cases {
		if got := Authorize(c.c, c.p, c.r); got.Allowed != c.want {
			t.Errorf("%s: got %v (%s) want %v", c.name, got.Allowed, got.Reason, c.want)
		}
	}
	if VisibleBucket(nil, "b") || !VisibleBucket(user(meta.RoleReadOnly, "b"), "b") || VisibleBucket(user(meta.RoleReadOnly, "b"), "c") || !VisibleBucket(user(meta.RoleAdmin), "c") {
		t.Fatal("VisibleBucket")
	}
	// Owner may change bucket configuration; another writer may not.
	owner := user(meta.RoleReadWrite)
	if !AuthorizeOwned(owner, nil, Request{Action: "s3:PutBucketPolicy", Bucket: "b"}, owner.User.ID).Allowed {
		t.Fatal("owner should be allowed")
	}
	if AuthorizeOwned(owner, nil, Request{Action: "s3:PutBucketPolicy", Bucket: "b"}, "someone-else").Allowed {
		t.Fatal("non-owner must be refused")
	}
	if !IsOwnerAction("s3:DeleteBucket") || IsOwnerAction("s3:GetObject") {
		t.Fatal("IsOwnerAction")
	}
	var anon *Caller
	if anon.Name() != "" || anon.IsAdmin() || !anon.Anonymous() || user(meta.RoleAdmin).Name() != "alice" {
		t.Fatal("caller helpers")
	}
}
