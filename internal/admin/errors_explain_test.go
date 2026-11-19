package admin

import "testing"

func TestExplainBucketProbeFromReplication(t *testing.T) {
	if _, sev := explain("NoSuchBucket", 404, "HeadBucket", true); sev != "info" {
		t.Fatalf("replication bucket probe: severity %q, want info", sev)
	}
	if _, sev := explain("NoSuchBucket", 404, "HeadBucket", false); sev != "warning" {
		t.Fatalf("client bucket probe: severity %q, want warning", sev)
	}
	if _, sev := explain("NoSuchBucket", 404, "GetObject", true); sev != "warning" {
		t.Fatalf("replicated read of a missing bucket: severity %q, want warning", sev)
	}
}
