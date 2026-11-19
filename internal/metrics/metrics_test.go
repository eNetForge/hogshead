package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetrics(t *testing.T) {
	m := New()
	m.Observe("GetObject", 200, 5*time.Millisecond)
	m.Observe("PutObject", 503, time.Second)
	m.BytesIn.Add(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Poll(ctx, time.Millisecond, func(_ context.Context, m *Metrics) { m.Objects.Set(42) })
	time.Sleep(5 * time.Millisecond)
	srv := httptest.NewServer(TokenHandler("secret", m.Handler()))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("token required")
	}
	resp, _ = http.Get(srv.URL + "?token=secret")
	if resp.StatusCode != 200 {
		t.Fatal("token query")
	}
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, _ = http.DefaultClient.Do(req)
	b := make([]byte, 1<<20)
	n, _ := resp.Body.Read(b)
	body := string(b[:n])
	for _, want := range []string{`hogshead_s3_requests_total{op="GetObject",status="2xx"} 1`, `hogshead_s3_requests_total{op="PutObject",status="5xx"} 1`, "hogshead_objects 42", "hogshead_s3_bytes_in_total 10"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	off := httptest.NewServer(TokenHandler("", m.Handler()))
	defer off.Close()
	resp, _ = http.Get(off.URL)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatal("disabled")
	}
}
