package discovery

import (
	"testing"
	"time"
)

func TestPeersTable(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(func() Beacon { return Beacon{ID: "me", Name: "self", Port: 9000} }, nil)
	s.now = func() time.Time { return now }
	s.Observe(Beacon{ID: "b", Name: "hogshead2", Port: 9000, TLS: true, Version: "1"}, "10.0.0.2")
	s.Observe(Beacon{ID: "a", Name: "hogshead1", Port: 9001}, "10.0.0.1")
	ps := s.Peers()
	if len(ps) != 2 || ps[0].Name != "hogshead1" || ps[0].Endpoint != "http://10.0.0.1:9001" || ps[1].Endpoint != "https://10.0.0.2:9000" {
		t.Fatalf("peers: %+v", ps)
	}
	// A beacon refreshes last_seen; stale peers expire after 4 intervals.
	now = now.Add(3 * Interval)
	s.Observe(Beacon{ID: "a", Name: "hogshead1", Port: 9001}, "10.0.0.1")
	now = now.Add(2 * Interval)
	s.expire()
	if ps := s.Peers(); len(ps) != 1 || ps[0].ID != "a" {
		t.Fatalf("expiry: %+v", ps)
	}
}
