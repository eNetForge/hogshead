// Package s3xml holds the Go representations of every S3 request and
// response XML body hogshead needs, plus Marshal/Unmarshal helpers that
// apply the S3 namespace and request-size limits.
//
// Response roots carry an `Xmlns` attribute field; Marshal fills it with NS
// when empty, so handlers never have to remember the namespace. Error
// documents are rendered by package s3err, not here.
package s3xml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3err"
)

// NS is the S3 XML namespace placed on every response root element.
const NS = "http://s3.amazonaws.com/doc/2006-03-01/"

// XsiNS is the XML Schema instance namespace used for Grantee xsi:type.
const XsiNS = "http://www.w3.org/2001/XMLSchema-instance"

// DefaultLimit is the request body limit Unmarshal applies when limit <= 0.
const DefaultLimit int64 = 1 << 20

// TimeFormat is the ISO 8601 layout S3 uses in XML bodies.
const TimeFormat = "2006-01-02T15:04:05.000Z"

const xmlnsField = "Xmlns"

// Marshal renders v as the XML declaration followed by an indented body.
// If v (or *v) is a struct with an empty string field named Xmlns, the
// namespace is set on a copy before encoding; v itself is never modified.
func Marshal(v any) ([]byte, error) {
	body, err := xml.MarshalIndent(withNamespace(v), "", "  ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Grow(len(xml.Header) + len(body) + 1)
	buf.WriteString(xml.Header)
	buf.Write(body)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// withNamespace returns v, or a shallow copy of v with Xmlns set to NS when
// the field exists and is empty.
func withNamespace(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return v
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return v
	}
	f := rv.FieldByName(xmlnsField)
	if !f.IsValid() || f.Kind() != reflect.String || f.String() != "" {
		return v
	}
	cp := reflect.New(rv.Type()).Elem()
	cp.Set(rv)
	cp.FieldByName(xmlnsField).SetString(NS)
	return cp.Interface()
}

// Unmarshal reads at most limit bytes from r (DefaultLimit when limit <= 0)
// and decodes them into v. Failures are reported as *s3err.Error:
// MissingRequestBodyError for an empty body, MaxMessageLengthExceeded when
// the body exceeds limit, and MalformedXML for anything the decoder rejects.
func Unmarshal(r io.Reader, v any, limit int64) error {
	if limit <= 0 {
		limit = DefaultLimit
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return s3err.Newf(s3err.IncompleteBody, "reading request body: %v", err)
	}
	if int64(len(data)) > limit {
		return s3err.New(s3err.MaxMessageLengthExceeded)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return s3err.New(s3err.MissingRequestBodyError)
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return s3err.Newf(s3err.MalformedXML, "%s: %v", s3err.New(s3err.MalformedXML).Message, err)
	}
	if err := rejectTrailingContent(dec); err != nil {
		return err
	}
	return nil
}

// rejectTrailingContent fails with MalformedXML if anything other than
// whitespace or comments follows the root element.
func rejectTrailingContent(dec *xml.Decoder) error {
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return s3err.New(s3err.MalformedXML)
		}
		switch t := tok.(type) {
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return s3err.New(s3err.MalformedXML)
			}
		case xml.Comment:
		default:
			return s3err.New(s3err.MalformedXML)
		}
	}
}

// FormatTime renders t in UTC as ISO 8601 with millisecond precision.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeFormat)
}

// ParseTime parses a timestamp produced by FormatTime (or the equivalent
// RFC 3339 form without fractional seconds).
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeFormat, s)
	if err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
