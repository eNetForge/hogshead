package ids

import "testing"

func TestIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		v := VersionID()
		if len(v) != 32 || v == "null" || seen[v] {
			t.Fatalf("version id %q", v)
		}
		seen[v] = true
	}
	if len(UploadID()) != 48 || len(AccessKeyID()) != 20 || len(SecretKey()) != 40 || len(RequestID()) != 16 || len(Token(4)) != 8 || UserID()[:2] != "u-" {
		t.Fatal("lengths")
	}
}
