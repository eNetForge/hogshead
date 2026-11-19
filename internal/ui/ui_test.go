package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestIndexAndFallback(t *testing.T) {
	h := Handler()
	for _, target := range []string{"/", "/_ui/", "/_ui", "/_ui/buckets/some-bucket", "/no/such/route", "/_ui/../../etc/passwd"} {
		t.Run(target, func(t *testing.T) {
			rec := get(t, h, target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("content-type = %q, want text/html", ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != cacheNoStore {
				t.Fatalf("cache-control = %q, want %q", cc, cacheNoStore)
			}
			if !strings.Contains(rec.Body.String(), "<html") {
				t.Fatalf("body does not look like index.html: %q", rec.Body.String()[:min(80, rec.Body.Len())])
			}
		})
	}
}

// firstAsset finds any non-index file in the embedded dist so the test works
// both with the placeholder and with a real build.
func firstAsset(t *testing.T) (string, bool) {
	t.Helper()
	var found string
	err := fs.WalkDir(dist, "dist", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" || p == "dist/index.html" {
			return nil
		}
		found = strings.TrimPrefix(p, "dist")
		return fs.SkipAll
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return found, found != ""
}

func TestServesExistingAsset(t *testing.T) {
	h := Handler()
	asset, ok := firstAsset(t)
	if !ok {
		// Placeholder build: index.html is the only file. Verify that it is
		// served by its explicit name instead.
		asset = "/index.html"
	}
	rec := get(t, h, Prefix+asset)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", asset, rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("GET %s: empty body", asset)
	}
	if ok && strings.HasPrefix(asset, immutableDir) {
		if cc := rec.Header().Get("Cache-Control"); cc != cacheImmutable {
			t.Fatalf("cache-control = %q, want %q", cc, cacheImmutable)
		}
	}
}

func TestImmutableCacheHeader(t *testing.T) {
	h := Handler()
	var asset string
	_ = fs.WalkDir(dist, "dist/_app/immutable", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || asset != "" {
			return nil
		}
		asset = strings.TrimPrefix(p, "dist")
		return fs.SkipAll
	})
	if asset == "" {
		t.Skip("no built assets embedded (placeholder dist)")
	}
	rec := get(t, h, Prefix+asset)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != cacheImmutable {
		t.Fatalf("cache-control = %q, want %q", cc, cacheImmutable)
	}
}

func TestMissingAppAssetIs404(t *testing.T) {
	rec := get(t, Handler(), Prefix+"/_app/immutable/chunks/missing.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (not the SPA fallback)", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_ui/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
