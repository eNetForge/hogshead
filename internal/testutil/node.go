// Package testutil boots a complete single node for integration tests.
package testutil

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/api"
	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/notify"
	"github.com/enetforge/hogshead/internal/server"
)

// Node is a running test node.
type Node struct {
	T      *testing.T
	Core   *server.Core
	Server *httptest.Server
	Admin  auth.Credentials // admin user key
	Cfg    config.Config
	Now    func() time.Time
	// API is the S3 handler behind Server, for tests that wire extra hooks.
	API *api.Handler
}

// Start boots a server on a temp directory with an admin key.
func Start(t *testing.T) *Node {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default(dir)
	cfg.Hostname = "s3.test"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("HOGSHEAD_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	ctx := context.Background()
	core, err := server.Open(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{T: t, Core: core, Cfg: cfg, Now: time.Now}
	n.Admin = n.CreateUser("admin", meta.RoleAdmin, nil)
	hooks := notify.New(func(ctx context.Context, name string) (string, string, bool) {
		u, err := core.DB.GetSetting(ctx, notify.SettingPrefix+name)
		if err != nil || u == "" {
			return "", "", false
		}
		secret, _ := core.DB.GetSetting(ctx, notify.SettingPrefix+name+".secret")
		return u, secret, true
	}, log, 1)
	h := api.New(api.Deps{DB: core.DB, Propose: core.ProposeRaw, Chunks: core.Chunks, Cfg: core.Cfg, Log: log, Region: "us-east-1", Notify: hooks})
	n.API = h
	n.Server = httptest.NewServer(h)
	t.Cleanup(func() {
		n.Server.Close()
		hooks.Close()
		_ = core.Close()
	})
	return n
}

// CreateUser adds a user with one access key and returns the credentials.
func (n *Node) CreateUser(name, role string, buckets []string) auth.Credentials {
	n.T.Helper()
	ctx := context.Background()
	uid := ids.UserID()
	if _, err := n.Core.Propose(ctx, &meta.CreateUser{ID: uid, Name: name, Role: role, Buckets: buckets, Now: time.Now().UnixMilli()}); err != nil {
		n.T.Fatal(err)
	}
	c := auth.Credentials{AccessKeyID: ids.AccessKeyID(), SecretKey: ids.SecretKey()}
	if _, err := n.Core.Propose(ctx, &meta.CreateAccessKey{AccessKeyID: c.AccessKeyID, Secret: c.SecretKey, UserID: uid, Now: time.Now().UnixMilli()}); err != nil {
		n.T.Fatal(err)
	}
	return c
}

// URL returns the base URL.
func (n *Node) URL() string { return n.Server.URL }

// Request builds a signed request. body may be nil. payloadHash "" means
// UNSIGNED-PAYLOAD; pass "sha256" to sign the actual body.
func (n *Node) Request(creds *auth.Credentials, method, path string, body []byte, headers map[string]string) *http.Request {
	n.T.Helper()
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, n.Server.URL+path, rd)
	if err != nil {
		n.T.Fatal(err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if creds != nil {
		hash := "UNSIGNED-PAYLOAD"
		if req.Header.Get("x-amz-content-sha256") == "" {
			req.Header.Set("x-amz-content-sha256", hash)
		}
		if err := auth.Sign(req, *creds, "us-east-1", "s3", n.Now(), ""); err != nil {
			n.T.Fatal(err)
		}
	}
	return req
}

// Do sends a request and returns the response with the body read.
func (n *Node) Do(req *http.Request) (*http.Response, []byte) {
	n.T.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.T.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// Call is Request + Do.
func (n *Node) Call(creds *auth.Credentials, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	n.T.Helper()
	return n.Do(n.Request(creds, method, path, body, headers))
}

// Expect fails the test unless the status matches.
func (n *Node) Expect(status int, resp *http.Response, body []byte) {
	n.T.Helper()
	if resp.StatusCode != status {
		n.T.Fatalf("%s %s: want %d got %d: %s", resp.Request.Method, resp.Request.URL.Path, status, resp.StatusCode, body)
	}
}

// DataDir returns the node's data dir.
func (n *Node) DataDir() string { return n.Cfg.DataDir }

// ChunkPath returns the path of a chunk file.
func (n *Node) ChunkPath(hash string) string {
	return filepath.Join(n.Cfg.DataDir, "chunks", hash[0:2], hash[2:4], hash)
}

// Must sends a request and fails unless the status matches.
func (n *Node) Must(status int, creds *auth.Credentials, method, path string, body []byte, headers map[string]string) {
	n.T.Helper()
	resp, b := n.Call(creds, method, path, body, headers) //nolint:bodyclose // Do closes the body
	n.Expect(status, resp, b)
}
