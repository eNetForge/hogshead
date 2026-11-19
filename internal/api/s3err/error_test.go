package s3err

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

func TestRegistryComplete(t *testing.T) {
	codes := Codes()
	if len(codes) != 82 {
		t.Fatalf("expected 82 registered codes, got %d", len(codes))
	}
	if !sort.SliceIsSorted(codes, func(i, j int) bool { return codes[i] < codes[j] }) {
		t.Fatal("Codes() is not sorted")
	}
	for _, c := range codes {
		e := registry[c]
		if e.Code != c {
			t.Errorf("%s: registry key does not match entry code %q", c, e.Code)
		}
		if e.Message == "" {
			t.Errorf("%s: empty default message", c)
		}
		if e.HTTPStatus < 300 || e.HTTPStatus > 599 {
			t.Errorf("%s: implausible status %d", c, e.HTTPStatus)
		}
		if http.StatusText(e.HTTPStatus) == "" {
			t.Errorf("%s: unknown status %d", c, e.HTTPStatus)
		}
		if !IsRegistered(c) {
			t.Errorf("%s: IsRegistered false", c)
		}
	}
}

func TestExpectedStatuses(t *testing.T) {
	want := map[Code]int{
		AccessDenied:                  403,
		AccountProblem:                403,
		AuthorizationHeaderMalformed:  400,
		BadDigest:                     400,
		BucketAlreadyExists:           409,
		BucketAlreadyOwnedByYou:       409,
		BucketNotEmpty:                409,
		ConditionalRequestConflict:    409,
		EntityTooLarge:                400,
		InternalError:                 500,
		InvalidAccessKeyID:            403,
		InvalidObjectState:            403,
		InvalidRange:                  416,
		RequestedRangeNotSatisfiable:  416,
		MethodNotAllowed:              405,
		MissingContentLength:          411,
		NoSuchBucket:                  404,
		NoSuchKey:                     404,
		NoSuchObjectLockConfiguration: 404,
		NotImplemented:                501,
		NotModified:                   304,
		PermanentRedirect:             301,
		PreconditionFailed:            412,
		Redirect:                      307,
		TemporaryRedirect:             307,
		RequestTimeTooSkewed:          403,
		SignatureDoesNotMatch:         403,
		ServiceUnavailable:            503,
		SlowDown:                      503,
		ReadOnly:                      503,
		NoSuchUser:                    404,
		UserAlreadyExists:             409,
		NoSuchAccessKey:               404,
		NoSuchNode:                    404,
		InvalidPolicyDocument:         400,
		MalformedPolicy:               400,
		XAmzContentSHA256Mismatch:     400,
	}
	for c, status := range want {
		if got := New(c).HTTPStatus; got != status {
			t.Errorf("%s: status %d, want %d", c, got, status)
		}
	}
	if string(InvalidAccessKeyID) != "InvalidAccessKeyId" {
		t.Errorf("InvalidAccessKeyID wire value = %q", InvalidAccessKeyID)
	}
	if New(ReadOnly).Message != "Server is read-only" {
		t.Errorf("ReadOnly message = %q", New(ReadOnly).Message)
	}
}

func TestNewUnknownCode(t *testing.T) {
	e := New("DefinitelyNotACode")
	if e.Code != InternalError || e.HTTPStatus != 500 {
		t.Fatalf("unknown code should map to InternalError, got %+v", e)
	}
}

func TestNewCopiesRegistry(t *testing.T) {
	before := registry[NoSuchKey]
	e := New(NoSuchKey)
	e.Message = "mutated"
	e.HTTPStatus = 1
	after := registry[NoSuchKey]
	if after.Message != before.Message || after.HTTPStatus != before.HTTPStatus {
		t.Fatal("New returned a value aliasing the registry entry")
	}
	if New(NoSuchKey).Message != before.Message {
		t.Fatal("second New sees mutation")
	}
}

func TestNewf(t *testing.T) {
	e := Newf(InvalidArgument, "bad %s=%d", "max-keys", -1)
	if e.Code != InvalidArgument || e.HTTPStatus != 400 {
		t.Fatalf("unexpected %+v", e)
	}
	if e.Message != "bad max-keys=-1" {
		t.Fatalf("message = %q", e.Message)
	}
	if New(InvalidArgument).Message != "Invalid Argument" {
		t.Fatal("Newf leaked message into registry")
	}
}

func TestWithDoesNotMutate(t *testing.T) {
	base := New(NoSuchBucket).WithExtra("A", "1")
	withRes := base.WithResource("/b")
	withReq := base.WithRequestID("req-1")
	withMsg := base.WithMessage("custom")
	withExtra := base.WithExtra("B", "2")

	if base.Resource != "" || base.RequestID != "" || base.Message != "The specified bucket does not exist." {
		t.Fatalf("base mutated: %+v", base)
	}
	if len(base.Extra) != 1 {
		t.Fatalf("base Extra mutated: %v", base.Extra)
	}
	if withRes.Resource != "/b" || withReq.RequestID != "req-1" || withMsg.Message != "custom" {
		t.Fatal("With* did not apply")
	}
	if withExtra.Extra["B"] != "2" || withExtra.Extra["A"] != "1" {
		t.Fatalf("WithExtra = %v", withExtra.Extra)
	}
	// The returned copies must be distinct pointers from each other and base.
	if withRes == base || withReq == base || withMsg == base || withExtra == base {
		t.Fatal("With* returned the receiver")
	}
	// Registry still pristine.
	if registry[NoSuchBucket].Extra != nil || registry[NoSuchBucket].Resource != "" {
		t.Fatal("registry entry mutated")
	}
}

func TestErrorStringAndIs(t *testing.T) {
	e := New(NoSuchKey).WithResource("/b/k")
	if got := e.Error(); got != "NoSuchKey: The specified key does not exist." {
		t.Fatalf("Error() = %q", got)
	}
	if (&Error{Code: NoSuchKey}).Error() != "NoSuchKey" {
		t.Fatal("Error() with empty message should be just the code")
	}

	wrapped := fmt.Errorf("handler: %w", e)
	if !errors.Is(wrapped, New(NoSuchKey)) {
		t.Fatal("errors.Is should match by code")
	}
	if errors.Is(wrapped, New(NoSuchBucket)) {
		t.Fatal("errors.Is should not match a different code")
	}
	if e.Is(errors.New("NoSuchKey")) {
		t.Fatal("Is should reject non-*Error targets")
	}
	var target *Error
	if !errors.As(wrapped, &target) || target.Resource != "/b/k" {
		t.Fatal("errors.As failed")
	}
}

func TestStatus(t *testing.T) {
	if (&Error{Code: "X"}).Status() != 500 {
		t.Fatal("zero HTTPStatus should fall back to 500")
	}
	if New(NoSuchKey).Status() != 404 {
		t.Fatal("Status should return HTTPStatus")
	}
}

func TestXMLShape(t *testing.T) {
	e := New(NoSuchKey).WithResource("/bucket/key").WithRequestID("4442587FB7D0A2F9")
	e.BucketName = "bucket"
	e.Key = "key"
	e.Method = "GET"
	e.Region = "us-east-1"
	e = e.WithExtra("Zeta", "z").WithExtra("Alpha", "a & <b>")

	got := string(e.XML())
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>` +
		`<Resource>/bucket/key</Resource><RequestId>4442587FB7D0A2F9</RequestId>` +
		`<BucketName>bucket</BucketName><Key>key</Key><Method>GET</Method><Region>us-east-1</Region>` +
		`<Alpha>a &amp; &lt;b&gt;</Alpha><Zeta>z</Zeta></Error>`
	if got != want {
		t.Fatalf("XML mismatch\n got: %s\nwant: %s", got, want)
	}

	// Round-trips through a generic decoder.
	var back struct {
		Code      string `xml:"Code"`
		Message   string `xml:"Message"`
		RequestID string `xml:"RequestId"`
		Alpha     string `xml:"Alpha"`
	}
	if err := xml.Unmarshal(e.XML(), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Code != "NoSuchKey" || back.RequestID != "4442587FB7D0A2F9" || back.Alpha != "a & <b>" {
		t.Fatalf("round trip = %+v", back)
	}
}

func TestXMLMinimal(t *testing.T) {
	got := string(New(AccessDenied).XML())
	if !strings.HasPrefix(got, xml.Header) {
		t.Fatalf("missing XML header: %s", got)
	}
	body := strings.TrimPrefix(got, xml.Header)
	if body != `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>` {
		t.Fatalf("minimal body = %s", body)
	}
	if strings.Contains(got, "Resource") || strings.Contains(got, "RequestId") {
		t.Fatal("empty optional fields must be omitted")
	}
}

func TestXMLEveryCode(t *testing.T) {
	for _, c := range Codes() {
		b := New(c).XML()
		var back struct {
			Code string `xml:"Code"`
		}
		if err := xml.Unmarshal(b, &back); err != nil {
			t.Fatalf("%s: invalid XML: %v", c, err)
		}
		if back.Code != string(c) {
			t.Fatalf("%s: rendered code %q", c, back.Code)
		}
	}
}

func TestXMLSkipsEmptyExtraKey(t *testing.T) {
	e := New(NoSuchKey).WithRequestID("r1").WithExtra("", "ignored").WithExtra("Condition", "If-None-Match")
	got := string(e.XML())
	if strings.Contains(got, "ignored") || strings.Contains(got, "<Extra>") {
		t.Fatalf("empty Extra key must be skipped, got %s", got)
	}
	if !strings.Contains(got, "<Condition>If-None-Match</Condition>") {
		t.Fatalf("missing Condition element: %s", got)
	}
}
