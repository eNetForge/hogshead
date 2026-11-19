package replication

import (
	"testing"

	"github.com/enetforge/hogshead/internal/meta"
)

func TestWantedKeepsCellarStateInsideTheCellar(t *testing.T) {
	w := New(Deps{}, 0)
	plain := Target{Name: "ext", SyncConfig: true}
	member := Target{Name: "cellar:x", SyncConfig: true, Cellar: true}
	cases := []struct {
		e             meta.ReplicationEntry
		plain, cellar bool
	}{
		{meta.ReplicationEntry{Op: meta.ReplicationSetting, Key: "cellar.members"}, false, true},
		{meta.ReplicationEntry{Op: meta.ReplicationSetting, Key: "uploads.max_age"}, true, true},
		{meta.ReplicationEntry{Op: meta.ReplicationUser, Key: CellarServiceUser}, false, true},
		{meta.ReplicationEntry{Op: meta.ReplicationUser, Key: "alice"}, true, true},
		{meta.ReplicationEntry{Op: meta.ReplicationKey, Key: "AK", VersionID: CellarServiceUser}, false, true},
		{meta.ReplicationEntry{Op: meta.ReplicationKey, Key: "AK", VersionID: "alice"}, true, true},
		{meta.ReplicationEntry{Op: meta.ReplicationPut, Key: "k", Origin: "s-other"}, false, false},
		{meta.ReplicationEntry{Op: meta.ReplicationBucket, Key: "policy"}, true, true},
	}
	for _, c := range cases {
		if got := w.wanted(plain, c.e); got != c.plain {
			t.Errorf("%s %s to plain target: got %v want %v", c.e.Op, c.e.Key, got, c.plain)
		}
		if got := w.wanted(member, c.e); got != c.cellar {
			t.Errorf("%s %s to cellar target: got %v want %v", c.e.Op, c.e.Key, got, c.cellar)
		}
	}
	none := Target{Name: "nosync"}
	if w.wanted(none, meta.ReplicationEntry{Op: meta.ReplicationUser, Key: "alice"}) {
		t.Error("users must not go to a target without config sync")
	}
}
