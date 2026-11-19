package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3xml"
)

func TestDispatch(t *testing.T) {
	var got atomic.Int32
	var last atomic.Value
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flaky" && fails.Add(1) == 1 {
			http.Error(w, "boom", 500)
			return
		}
		b, _ := io.ReadAll(r.Body)
		last.Store(string(b) + "|" + r.Header.Get("X-Hogshead-Signature"))
		got.Add(1)
	}))
	defer srv.Close()
	lookup := func(_ context.Context, name string) (string, string, bool) {
		switch name {
		case "hook":
			return srv.URL + "/ok", "secret", true
		case "flaky":
			return srv.URL + "/flaky", "", true
		}
		return "", "", false
	}
	d := New(lookup, nil, 1)
	cfg := &s3xml.NotificationConfiguration{QueueConfiguration: []s3xml.QueueConfiguration{
		{ID: "all", Queue: ARNPrefix + "hook", Event: []string{"s3:ObjectCreated:*"}},
		{ID: "imgs", Queue: ARNPrefix + "flaky", Event: []string{"s3:ObjectRemoved:Delete"}, Filter: &s3xml.NotificationFilter{}},
		{ID: "other", Queue: "arn:aws:sqs:::real", Event: []string{"s3:*"}},
		{ID: "unknown", Queue: ARNPrefix + "nope", Event: []string{"s3:*"}},
	}}
	cfg.QueueConfiguration[1].Filter.S3Key.FilterRule = []s3xml.FilterRule{{Name: "prefix", Value: "img/"}, {Name: "suffix", Value: ".png"}}
	d.Emit(cfg, Event{Name: "s3:ObjectCreated:Put", Bucket: "b", Key: "a b.txt", Size: 3, ETag: `"e"`, Time: time.Now(), Principal: "alice", SourceIP: "1.2.3.4"})
	d.Emit(cfg, Event{Name: "s3:ObjectRemoved:Delete", Bucket: "b", Key: "img/x.png", Time: time.Now()})
	d.Emit(cfg, Event{Name: "s3:ObjectRemoved:Delete", Bucket: "b", Key: "doc/x.png", Time: time.Now()}) // filtered out
	d.Emit(nil, Event{})
	deadline := time.Now().Add(10 * time.Second)
	for got.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got.Load() != 2 {
		t.Fatalf("deliveries: %d", got.Load())
	}
	s, _ := last.Load().(string)
	if !strings.Contains(s, "ObjectRemoved:Delete") && !strings.Contains(s, "ObjectCreated:Put") {
		t.Fatalf("payload: %s", s)
	}
	var p Payload
	body := s[:strings.LastIndex(s, "|")]
	if err := json.Unmarshal([]byte(body), &p); err != nil || len(p.Records) != 1 || p.Records[0].S3.Bucket.Name != "b" {
		t.Fatalf("payload shape: %s", body)
	}
	d.Close()
	if !ValidEvent("s3:ObjectCreated:*") || ValidEvent("s3:Nope") {
		t.Fatal("ValidEvent")
	}
	q := &s3xml.QueueConfiguration{Event: []string{"s3:ObjectCreated:Put"}}
	if !Matches(q, "s3:ObjectCreated:Put", "k") || Matches(q, "s3:ObjectCreated:Copy", "k") {
		t.Fatal("Matches")
	}
	rec := buildRecord(Event{Name: "s3:ObjectCreated:Put", Key: "a/b c", Time: time.Unix(1, 0)}, "id")
	if rec.S3.Object.Key != "a%2Fb+c" || rec.EventName != "ObjectCreated:Put" || rec.S3.Object.Sequencer == "" {
		t.Fatalf("record: %+v", rec)
	}
}
