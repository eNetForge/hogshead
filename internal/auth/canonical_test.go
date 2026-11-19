package auth

import (
	"net/http"
	"net/url"
	"testing"
)

func TestURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", true, "-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"},
		{"a b", true, "a%20b"},
		{"a+b", true, "a%2Bb"},
		{"a/b", false, "a/b"},
		{"a/b", true, "a%2Fb"},
		{"ሴ", true, "%E1%88%B4"},
		{"*'()!", true, "%2A%27%28%29%21"},
		{"", true, ""},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.encodeSlash); got != c.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", c.in, c.encodeSlash, got, c.want)
		}
	}
}

func TestCanonicalURI(t *testing.T) {
	cases := []struct{ target, want string }{
		{"http://h", "/"},
		{"http://h/", "/"},
		{"http://h/test.txt", "/test.txt"},
		{"http://h/bucket/my%20key.txt", "/bucket/my%20key.txt"},
		{"http://h/bucket/my key.txt", "/bucket/my%20key.txt"},
		{"http://h/bucket/a+b.txt", "/bucket/a%2Bb.txt"},
		{"http://h/bucket/a%2Bb.txt", "/bucket/a%2Bb.txt"},
		{"http://h/bucket/ሴ/日本", "/bucket/%E1%88%B4/%E6%97%A5%E6%9C%AC"},
		{"http://h/bucket/%E1%88%B4", "/bucket/%E1%88%B4"},
		{"http://h/bucket/a%2Fb", "/bucket/a%2Fb"},
		{"http://h//double//slash", "//double//slash"},
		{"http://h/bucket/star*(1)", "/bucket/star%2A%281%29"},
		{"http://h/bucket/pct%25", "/bucket/pct%25"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.target)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalURI(u); got != c.want {
			t.Errorf("canonicalURI(%q) = %q, want %q", c.target, got, c.want)
		}
	}
}

func TestCanonicalQuery(t *testing.T) {
	cases := []struct{ raw, exclude, want string }{
		{"", "", ""},
		{"Param2=value2&Param1=value1", "", "Param1=value1&Param2=value2"},
		{"Param1=value1", "", "Param1=value1"},
		{"Param1", "", "Param1="},
		{"ሴ=bar", "", "%E1%88%B4=bar"},
		{"%E1%88%B4=bar", "", "%E1%88%B4=bar"},
		{"a=1+2", "", "a=1%202"},
		{"a=1%2B2", "", "a=1%2B2"},
		{"a=b=c", "", "a=b%3Dc"},
		{"k=2&k=1&k=10", "", "k=1&k=10&k=2"},
		{"b=1&a=2&&", "", "a=2&b=1"},
		{"prefix=foo/bar&delimiter=%2F", "", "delimiter=%2F&prefix=foo%2Fbar"},
		{"X-Amz-Signature=abc&a=1", "X-Amz-Signature", "a=1"},
		{"x-amz-signature=abc&a=1", "X-Amz-Signature", "a=1"},
		{"uploads&max-keys=5", "", "max-keys=5&uploads="},
		{"a=%zz", "", "a=%25zz"},
	}
	for _, c := range cases {
		if got := canonicalQuery(c.raw, c.exclude); got != c.want {
			t.Errorf("canonicalQuery(%q, %q) = %q, want %q", c.raw, c.exclude, got, c.want)
		}
	}
}

func TestTrimHeaderValue(t *testing.T) {
	cases := map[string]string{
		"  a   b  ":       "a b",
		"value1\t value2": "value1 value2",
		"":                "",
		"x":               "x",
	}
	for in, want := range cases {
		if got := trimHeaderValue(in); got != want {
			t.Errorf("trimHeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeaderValueFallbacks(t *testing.T) {
	r := suiteRequest(t, "PUT", "/", nil)
	r.ContentLength = 42
	r.TransferEncoding = []string{"chunked"}
	r.Header.Add("My-Header1", " value2 ")
	r.Header.Add("My-Header1", "value2")
	r.Header.Add("My-Header1", "value1")
	r.Header["x-odd_key"] = []string{"v"}

	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"host", suiteHost, true},
		{"content-length", "42", true},
		{"transfer-encoding", "chunked", true},
		{"expect", "100-continue", true},
		{"my-header1", "value2,value2,value1", true},
		{"x-odd_key", "v", true},
		{"x-missing", "", false},
	}
	for _, c := range cases {
		got, ok := headerValue(r, c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("headerValue(%q) = (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}

	r.Host = ""
	r.URL.Host = "from-url.example"
	if got, ok := headerValue(r, "host"); got != "from-url.example" || !ok {
		t.Errorf("host fallback to URL.Host: got (%q, %v)", got, ok)
	}
	r.URL.Host = ""
	if _, ok := headerValue(r, "host"); ok {
		t.Error("host with neither r.Host nor URL.Host should be missing")
	}
	r.ContentLength = -1
	r.TransferEncoding = nil
	if _, ok := headerValue(r, "content-length"); ok {
		t.Error("content-length unknown should be missing")
	}
	if _, ok := headerValue(r, "transfer-encoding"); ok {
		t.Error("transfer-encoding absent should be missing")
	}
}

func TestIsHex(t *testing.T) {
	if !isHex("0aF9", 4) || isHex("0aG9", 4) || isHex("abc", 4) {
		t.Error("isHex misclassified input")
	}
}

func TestPayloadModeOf(t *testing.T) {
	cases := []struct {
		in   string
		want PayloadMode
		err  error
	}{
		{"", PayloadUnsigned, nil},
		{unsignedPayload, PayloadUnsigned, nil},
		{emptySHA256, PayloadSigned, nil},
		{streamingSigned, PayloadStreaming, nil},
		{streamingSignedTrailer, PayloadStreamingTrailer, nil},
		{streamingUnsignedTrailer, PayloadStreamingUnsignedTrailer, nil},
		{"STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD", 0, ErrUnsupported},
		{"not-a-hash", 0, ErrMalformed},
	}
	for _, c := range cases {
		got, err := payloadModeOf(c.in)
		if c.err != nil {
			wantErr(t, err, c.err)
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("payloadModeOf(%q) = (%v, %v), want %v", c.in, got, err, c.want)
		}
	}
}

func TestPayloadModeString(t *testing.T) {
	for _, m := range []PayloadMode{PayloadSigned, PayloadUnsigned, PayloadStreaming, PayloadStreamingTrailer, PayloadStreamingUnsignedTrailer} {
		if m.String() == "" || m.String() == "PayloadMode(unknown)" {
			t.Errorf("%d has no name", int(m))
		}
	}
	if PayloadMode(99).String() != "PayloadMode(unknown)" {
		t.Error("unknown mode name")
	}
	if PayloadSigned.Streaming() || !PayloadStreamingTrailer.Streaming() {
		t.Error("Streaming misclassified")
	}
}

// suiteVector is one case of the AWS SigV4 test suite: the request plus the
// expected canonical request and Authorization header.
type suiteVector struct {
	name          string
	method        string
	target        string
	headers       map[string]string
	payloadHash   string // "" means empty body
	signedHeaders string
	creq          string
	signature     string
}

var suiteVectors = []suiteVector{
	{
		name: "get-vanilla", method: "GET", target: "/",
		signedHeaders: "host;x-amz-date",
		creq:          "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA256,
		signature:     "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
	},
	{
		name: "get-vanilla-query-order-key-case", method: "GET", target: "/?Param2=value2&Param1=value1",
		signedHeaders: "host;x-amz-date",
		creq:          "GET\n/\nParam1=value1&Param2=value2\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA256,
		signature:     "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
	},
	{
		name: "get-vanilla-empty-query-key", method: "GET", target: "/?Param1=value1",
		signedHeaders: "host;x-amz-date",
		signature:     "a67d582fa61cc504c4bae71f336f98b97f1ea3c7a6bfe1b6e45aec72011b9aeb",
	},
	{
		name: "get-vanilla-query-unreserved", method: "GET",
		target:        "/?-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz=-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
		signedHeaders: "host;x-amz-date",
		signature:     "9c3e54bfcdf0b19771a7f523ee5669cdf59bc7cc0884027167c21bb143a40197",
	},
	{
		name: "get-vanilla-utf8-query", method: "GET", target: "/?ሴ=bar",
		signedHeaders: "host;x-amz-date",
		creq:          "GET\n/\n%E1%88%B4=bar\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA256,
		signature:     "2cdec8eed098649ff3a119c94853b13c643bcf08f8b0a1d91e12c9027818dd04",
	},
	{
		name: "get-vanilla-utf8-query-preencoded", method: "GET", target: "/?%E1%88%B4=bar",
		signedHeaders: "host;x-amz-date",
		signature:     "2cdec8eed098649ff3a119c94853b13c643bcf08f8b0a1d91e12c9027818dd04",
	},
	{
		name: "get-unreserved", method: "GET", target: "/-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
		signedHeaders: "host;x-amz-date",
		signature:     "07ef7494c76fa4850883e2b006601f940f8a34d404d0cfa977f52a65bbf5f24f",
	},
	{
		name: "get-utf8", method: "GET", target: "/ሴ",
		signedHeaders: "host;x-amz-date",
		creq:          "GET\n/%E1%88%B4\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA256,
		signature:     "8318018e0b0f223aa2bbf98705b62bb787dc9c0e678f255a891fd03141be5d85",
	},
	{
		name: "get-header-key-duplicate", method: "GET", target: "/",
		signedHeaders: "host;my-header1;x-amz-date",
		creq:          "GET\n/\n\nhost:example.amazonaws.com\nmy-header1:value2,value2,value1\nx-amz-date:20150830T123600Z\n\nhost;my-header1;x-amz-date\n" + emptySHA256,
		signature:     "c9d5ea9f3f72853aea855b47ea873832890dbdd183b4468f858259531a5138ea",
	},
	{
		name: "post-vanilla-query", method: "POST", target: "/?Param1=value1",
		signedHeaders: "host;x-amz-date",
		signature:     "28038455d6de14eafc1f9222cf5aa6f1a96197d7deb8263271d420d138af7f11",
	},
	{
		name: "post-x-www-form-urlencoded", method: "POST", target: "/",
		headers:       map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		payloadHash:   sha256Hex([]byte("Param1=value1")),
		signedHeaders: "content-type;host;x-amz-date",
		creq: "POST\n/\n\ncontent-type:application/x-www-form-urlencoded\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\n" +
			"content-type;host;x-amz-date\n9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
		signature: "ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
	},
}

func (sv suiteVector) request(t *testing.T) *http.Request {
	t.Helper()
	r := suiteRequest(t, sv.method, sv.target, sv.headers)
	if sv.name == "get-header-key-duplicate" {
		r.Header["My-Header1"] = []string{"value2", "value2", "value1"}
	}
	hash := sv.payloadHash
	if hash == "" {
		hash = emptySHA256
	}
	// The suite predates x-amz-content-sha256; supply it unsigned so Verify
	// uses the same payload hash the vectors were generated with.
	r.Header.Set(headerContentSHA256, hash)
	r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential="+suiteAccessKey+"/20150830/us-east-1/service/aws4_request, SignedHeaders="+sv.signedHeaders+", Signature="+sv.signature)
	return r
}

func TestSuiteCanonicalRequest(t *testing.T) {
	for _, sv := range suiteVectors {
		if sv.creq == "" {
			continue
		}
		t.Run(sv.name, func(t *testing.T) {
			r := sv.request(t)
			params, err := parseAuthorization(r.Header.Get(headerAuthorization))
			if err != nil {
				t.Fatal(err)
			}
			got, err := canonicalRequest(r, params.signedHeaders, r.Header.Get(headerContentSHA256), "")
			if err != nil {
				t.Fatal(err)
			}
			if got != sv.creq {
				t.Errorf("canonical request mismatch\n got: %q\nwant: %q", got, sv.creq)
			}
		})
	}
}

func TestSuiteVerify(t *testing.T) {
	v := suiteVerifier(suiteTime)
	for _, sv := range suiteVectors {
		t.Run(sv.name, func(t *testing.T) {
			id := mustVerify(t, v, sv.request(t))
			if id.AccessKeyID != suiteAccessKey || id.Region != suiteRegion || id.Service != suiteService {
				t.Errorf("identity = %+v", id)
			}
			if id.Scope != "20150830/us-east-1/service/aws4_request" || id.SeedSignature != sv.signature {
				t.Errorf("scope/seed = %q/%q", id.Scope, id.SeedSignature)
			}
			if !id.Date.Equal(suiteTime) || id.Presigned {
				t.Errorf("date/presigned = %v/%v", id.Date, id.Presigned)
			}
		})
	}
}

func TestSuiteStringToSign(t *testing.T) {
	sv := suiteVectors[0]
	got := stringToSign(suiteTime, "20150830/us-east-1/service/aws4_request", sv.creq)
	want := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"
	if got != want {
		t.Errorf("stringToSign\n got: %q\nwant: %q", got, want)
	}
	key := signingKey(suiteSecret, suiteTime, suiteRegion, suiteService)
	if sig := signHex(key, got); sig != sv.signature {
		t.Errorf("signature = %s, want %s", sig, sv.signature)
	}
}
