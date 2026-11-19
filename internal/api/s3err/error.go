// Package s3err defines the S3 error codes, their default messages and HTTP
// statuses, and renders the standard S3 <Error> XML body.
//
// Every code is held in an immutable registry; New returns a copy, and the
// fluent With* methods always return fresh copies, so callers can never
// mutate a registry entry.
package s3err

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
)

// Code is an S3 error code as it appears in the <Code> element.
type Code string

// Error is an S3-level error. It carries the HTTP status and every detail
// field that may be rendered in the XML <Error> body.
type Error struct {
	Code       Code
	Message    string
	HTTPStatus int

	// Optional detail fields rendered when non-empty.
	Resource   string
	RequestID  string
	BucketName string
	Key        string
	Method     string
	Region     string

	// Extra adds arbitrary child elements, e.g. {"Condition": "If-None-Match"}.
	Extra map[string]string
}

// Error implements the error interface as "Code: Message".
func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Is reports whether target is an *Error with the same Code, so errors.Is
// can compare against New(code) sentinels.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// New returns a copy of the registered error for code with its default
// message and status. Unknown codes become InternalError.
func New(code Code) *Error {
	tmpl, ok := registry[code]
	if !ok {
		tmpl = registry[InternalError]
	}
	return &Error{Code: tmpl.Code, Message: tmpl.Message, HTTPStatus: tmpl.HTTPStatus}
}

// Newf returns New(code) with a formatted message replacing the default.
func Newf(code Code, format string, args ...any) *Error {
	return New(code).WithMessage(fmt.Sprintf(format, args...))
}

// clone returns a deep copy of e (the Extra map is copied as well).
func (e *Error) clone() *Error {
	cp := *e
	if e.Extra != nil {
		cp.Extra = make(map[string]string, len(e.Extra)+1)
		for k, v := range e.Extra {
			cp.Extra[k] = v
		}
	}
	return &cp
}

// WithResource returns a copy with Resource set.
func (e *Error) WithResource(r string) *Error {
	cp := e.clone()
	cp.Resource = r
	return cp
}

// WithRequestID returns a copy with RequestID set.
func (e *Error) WithRequestID(id string) *Error {
	cp := e.clone()
	cp.RequestID = id
	return cp
}

// WithMessage returns a copy with Message set.
func (e *Error) WithMessage(m string) *Error {
	cp := e.clone()
	cp.Message = m
	return cp
}

// WithExtra returns a copy with the extra child element k=v added.
func (e *Error) WithExtra(k, v string) *Error {
	cp := e.clone()
	if cp.Extra == nil {
		cp.Extra = make(map[string]string, 1)
	}
	cp.Extra[k] = v
	return cp
}

// Status returns the HTTP status, falling back to 500 when unset.
func (e *Error) Status() int {
	if e.HTTPStatus == 0 {
		return http.StatusInternalServerError
	}
	return e.HTTPStatus
}

// extraElem renders one Extra entry as <k>v</k>.
type extraElem struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

// errorXML is the wire shape of the S3 <Error> document.
type errorXML struct {
	XMLName    xml.Name    `xml:"Error"`
	Code       Code        `xml:"Code"`
	Message    string      `xml:"Message"`
	Resource   string      `xml:"Resource,omitempty"`
	RequestID  string      `xml:"RequestId,omitempty"`
	BucketName string      `xml:"BucketName,omitempty"`
	Key        string      `xml:"Key,omitempty"`
	Method     string      `xml:"Method,omitempty"`
	Region     string      `xml:"Region,omitempty"`
	Extra      []extraElem `xml:",omitempty"`
}

// XML renders the standard S3 error body:
//
//	<?xml version="1.0" encoding="UTF-8"?>
//	<Error><Code>..</Code><Message>..</Message><Resource>..</Resource><RequestId>..</RequestId>...</Error>
//
// Extra elements are emitted in sorted key order so output is deterministic;
// entries with an empty key are skipped.
func (e *Error) XML() []byte {
	doc := errorXML{
		Code:       e.Code,
		Message:    e.Message,
		Resource:   e.Resource,
		RequestID:  e.RequestID,
		BucketName: e.BucketName,
		Key:        e.Key,
		Method:     e.Method,
		Region:     e.Region,
	}
	if len(e.Extra) > 0 {
		keys := make([]string, 0, len(e.Extra))
		for k := range e.Extra {
			if k != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		doc.Extra = make([]extraElem, 0, len(keys))
		for _, k := range keys {
			doc.Extra = append(doc.Extra, extraElem{XMLName: xml.Name{Local: k}, Value: e.Extra[k]})
		}
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	body, err := xml.Marshal(doc)
	if err != nil {
		// encoding/xml cannot fail on this fixed, string-only shape; guard
		// anyway so a response body is always well formed.
		body = []byte("<Error><Code>InternalError</Code><Message>" + registry[InternalError].Message + "</Message></Error>")
	}
	buf.Write(body)
	return buf.Bytes()
}
