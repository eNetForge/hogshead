// Package adminclient is the Go client for the admin API, used by the CLI.
package adminclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/discovery"
	"github.com/enetforge/hogshead/internal/replication"
	"github.com/enetforge/hogshead/internal/transfer"
)

// Client talks to one node's admin API.
type Client struct {
	base  string
	creds *auth.Credentials
	http  *http.Client
	user  string // identity hint over the socket
}

// NewSocket connects over the local unix socket.
func NewSocket(path string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	return &Client{base: "http://hogshead.local", http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
}

// NewHTTP connects to a server URL with SigV4 credentials.
func NewHTTP(serverURL string, creds auth.Credentials) *Client {
	return &Client{base: strings.TrimRight(serverURL, "/"), creds: &creds, http: &http.Client{Timeout: 60 * time.Second}}
}

// WithUser sets the audit identity used over the socket.
func (c *Client) WithUser(name string) *Client { c.user = name; return c }

// Error is a non-2xx response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	var raw []byte
	if in != nil {
		var err error
		raw, err = json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+admin.Prefix+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(raw))
	}
	if c.user != "" {
		req.Header.Set("X-Hogshead-User", c.user)
	}
	if c.creds != nil {
		req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
		if err := auth.Sign(req, *c.creds, "us-east-1", "s3", time.Now(), ""); err != nil {
			return err
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var eb admin.ErrorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error.Code != "" {
			return &Error{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
		}
		return &Error{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Status fetches node status.
func (c *Client) Status(ctx context.Context) (*admin.Status, error) {
	var s admin.Status
	return &s, c.do(ctx, "GET", "/status", nil, &s)
}

// Me fetches the caller identity.
func (c *Client) Me(ctx context.Context) (*admin.Me, error) {
	var m admin.Me
	return &m, c.do(ctx, "GET", "/me", nil, &m)
}

// ListUsers lists users.
func (c *Client) ListUsers(ctx context.Context) ([]admin.User, error) {
	var u []admin.User
	return u, c.do(ctx, "GET", "/users", nil, &u)
}

// CreateUser creates a user.
func (c *Client) CreateUser(ctx context.Context, req admin.CreateUserRequest) (*admin.User, error) {
	var u admin.User
	return &u, c.do(ctx, "POST", "/users", req, &u)
}

// GetUser fetches a user.
func (c *Client) GetUser(ctx context.Context, name string) (*admin.User, error) {
	var u admin.User
	return &u, c.do(ctx, "GET", "/users/"+url.PathEscape(name), nil, &u)
}

// UpdateUser updates a user.
// SetUserPassword sets (or with "" clears) a user's web UI password.
func (c *Client) SetUserPassword(ctx context.Context, name, password string) (*admin.User, error) {
	var u admin.User
	return &u, c.do(ctx, "PUT", "/users/"+url.PathEscape(name)+"/password", admin.PasswordRequest{Password: password}, &u)
}

func (c *Client) UpdateUser(ctx context.Context, name string, req admin.UpdateUserRequest) (*admin.User, error) {
	var u admin.User
	return &u, c.do(ctx, "PATCH", "/users/"+url.PathEscape(name), req, &u)
}

// DeleteUser deletes a user.
func (c *Client) DeleteUser(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/users/"+url.PathEscape(name), nil, nil)
}

// ListKeys lists a user's keys, or all keys when user is empty.
func (c *Client) ListKeys(ctx context.Context, user string) ([]admin.AccessKey, error) {
	var k []admin.AccessKey
	if user == "" {
		return k, c.do(ctx, "GET", "/keys", nil, &k)
	}
	return k, c.do(ctx, "GET", "/users/"+url.PathEscape(user)+"/keys", nil, &k)
}

// CreateKey creates a key for a user.
func (c *Client) CreateKey(ctx context.Context, user, label string) (*admin.CreatedKey, error) {
	var k admin.CreatedKey
	return &k, c.do(ctx, "POST", "/users/"+url.PathEscape(user)+"/keys", admin.CreateKeyRequest{Label: label}, &k)
}

// UpdateKey enables or disables a key.
func (c *Client) UpdateKey(ctx context.Context, id string, disabled bool) (*admin.AccessKey, error) {
	var k admin.AccessKey
	return &k, c.do(ctx, "PATCH", "/keys/"+url.PathEscape(id), admin.UpdateKeyRequest{Disabled: disabled}, &k)
}

// DeleteKey revokes a key.
func (c *Client) DeleteKey(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/keys/"+url.PathEscape(id), nil, nil)
}

// ListBuckets lists buckets.
func (c *Client) ListBuckets(ctx context.Context) ([]admin.Bucket, error) {
	var b []admin.Bucket
	return b, c.do(ctx, "GET", "/buckets", nil, &b)
}

// CreateBucket creates a bucket.
func (c *Client) CreateBucket(ctx context.Context, req admin.CreateBucketRequest) (*admin.Bucket, error) {
	var b admin.Bucket
	return &b, c.do(ctx, "POST", "/buckets", req, &b)
}

// GetBucket fetches a bucket.
func (c *Client) GetBucket(ctx context.Context, name string) (*admin.Bucket, error) {
	var b admin.Bucket
	return &b, c.do(ctx, "GET", "/buckets/"+url.PathEscape(name), nil, &b)
}

// UpdateBucket updates bucket settings.
func (c *Client) UpdateBucket(ctx context.Context, name string, req admin.UpdateBucketRequest) (*admin.Bucket, error) {
	var b admin.Bucket
	return &b, c.do(ctx, "PATCH", "/buckets/"+url.PathEscape(name), req, &b)
}

// DeleteBucket deletes a bucket.
func (c *Client) DeleteBucket(ctx context.Context, name string, force bool) error {
	q := ""
	if force {
		q = "?force=true"
	}
	return c.do(ctx, "DELETE", "/buckets/"+url.PathEscape(name)+q, nil, nil)
}

// Audit fetches audit entries.
func (c *Client) Audit(ctx context.Context, limit int, before int64) ([]admin.AuditEntry, error) {
	var a []admin.AuditEntry
	return a, c.do(ctx, "GET", fmt.Sprintf("/audit?limit=%d&before=%d", limit, before), nil, &a)
}

// Settings lists server settings.
func (c *Client) Settings(ctx context.Context) ([]admin.Setting, error) {
	var s []admin.Setting
	return s, c.do(ctx, "GET", "/settings", nil, &s)
}

// SetSetting sets a server setting.
func (c *Client) SetSetting(ctx context.Context, key, value string) error {
	return c.do(ctx, "PUT", "/settings/"+url.PathEscape(key), admin.SettingRequest{Value: value}, nil)
}

// Config fetches the node config.
func (c *Client) Config(ctx context.Context) (*admin.ConfigView, error) {
	var v admin.ConfigView
	return &v, c.do(ctx, "GET", "/config", nil, &v)
}

// ReloadConfig reloads the node config.
func (c *Client) ReloadConfig(ctx context.Context) (*admin.ConfigView, error) {
	var v admin.ConfigView
	return &v, c.do(ctx, "POST", "/config/reload", nil, &v)
}

// RotateSessions replaces the web UI session secret, signing every web UI
// session out.
func (c *Client) RotateSessions(ctx context.Context) error {
	return c.do(ctx, "POST", "/sessions/rotate", nil, nil)
}

// OpenAPI fetches the API description.
func (c *Client) OpenAPI(ctx context.Context) (map[string]any, error) {
	var v map[string]any
	return v, c.do(ctx, "GET", "/openapi.json", nil, &v)
}

// Raw performs an arbitrary call.
func (c *Client) Raw(ctx context.Context, method, path string, in, out any) error {
	return c.do(ctx, method, path, in, out)
}

// NewPlain connects over HTTP without credentials; only useful against a
// trusted (socket) handler exposed for tests.
func NewPlain(serverURL string) *Client {
	return &Client{base: strings.TrimRight(serverURL, "/"), http: &http.Client{Timeout: 60 * time.Second}}
}

// Replication lists every replication target with status.
func (c *Client) Replication(ctx context.Context) ([]replication.TargetStatus, error) {
	var v []replication.TargetStatus
	return v, c.do(ctx, "GET", "/replication", nil, &v)
}

// GetReplication fetches one bucket's targets.
func (c *Client) GetReplication(ctx context.Context, bucket string) ([]replication.TargetStatus, error) {
	var v []replication.TargetStatus
	return v, c.do(ctx, "GET", "/buckets/"+url.PathEscape(bucket)+"/replication", nil, &v)
}

// SetReplication adds or updates a target; target may be empty to derive it
// from the endpoint host.
func (c *Client) SetReplication(ctx context.Context, bucket, target string, req admin.ReplicationRequest) ([]replication.TargetStatus, error) {
	var v []replication.TargetStatus
	q := ""
	if target != "" {
		q = "?target=" + url.QueryEscape(target)
	}
	return v, c.do(ctx, "PUT", "/buckets/"+url.PathEscape(bucket)+"/replication"+q, req, &v)
}

// ImportUser creates or replaces a user wholesale.
func (c *Client) ImportUser(ctx context.Context, name string, req admin.UserImportRequest) (*admin.User, error) {
	var u admin.User
	return &u, c.do(ctx, "PUT", "/users/"+url.PathEscape(name), req, &u)
}

// ImportKey creates or replaces an access key with a known secret.
func (c *Client) ImportKey(ctx context.Context, user, ak string, req admin.KeyImportRequest) (*admin.AccessKey, error) {
	var k admin.AccessKey
	return &k, c.do(ctx, "PUT", "/users/"+url.PathEscape(user)+"/keys/"+url.PathEscape(ak), req, &k)
}

// Peers lists hogshead servers heard on the LAN.
func (c *Client) Peers(ctx context.Context) ([]discovery.Peer, error) {
	var v []discovery.Peer
	return v, c.do(ctx, "GET", "/peers", nil, &v)
}

// Usage queries S3 usage counters.
func (c *Client) Usage(ctx context.Context, params url.Values) (*admin.UsageResponse, error) {
	var v admin.UsageResponse
	q := ""
	if len(params) > 0 {
		q = "?" + params.Encode()
	}
	return &v, c.do(ctx, "GET", "/usage"+q, nil, &v)
}

// Errors lists recent failed S3 requests grouped by cause.
func (c *Client) Errors(ctx context.Context, params url.Values) (*admin.ErrorsResponse, error) {
	var v admin.ErrorsResponse
	q := ""
	if len(params) > 0 {
		q = "?" + params.Encode()
	}
	return &v, c.do(ctx, "GET", "/errors"+q, nil, &v)
}

// Cellar returns cellar membership and state.
func (c *Client) Cellar(ctx context.Context) (*cellar.Status, error) {
	var v cellar.Status
	return &v, c.do(ctx, "GET", "/cellar", nil, &v)
}

// CellarInit creates a cellar.
func (c *Client) CellarInit(ctx context.Context, req admin.CellarInitRequest) (*cellar.Status, error) {
	var v cellar.Status
	return &v, c.do(ctx, "POST", "/cellar/init", req, &v)
}

// CellarUpdate changes the name or write acknowledgement level.
func (c *Client) CellarUpdate(ctx context.Context, req admin.CellarUpdateRequest) (*cellar.Status, error) {
	var v cellar.Status
	return &v, c.do(ctx, "PUT", "/cellar", req, &v)
}

// CellarJoin asks a member to admit this server.
func (c *Client) CellarJoin(ctx context.Context, req admin.CellarJoinRequest) (*cellar.Status, error) {
	var v cellar.Status
	return &v, c.do(ctx, "POST", "/cellar/join", req, &v)
}

// CellarCancelJoin withdraws an outstanding join request.
func (c *Client) CellarCancelJoin(ctx context.Context) error {
	return c.do(ctx, "DELETE", "/cellar/join", nil, nil)
}

// CellarApprove approves a pending join request.
func (c *Client) CellarApprove(ctx context.Context, id string) (*cellar.Status, error) {
	var v cellar.Status
	return &v, c.do(ctx, "POST", "/cellar/pending/"+url.PathEscape(id)+"/approve", nil, &v)
}

// CellarReject rejects a pending join request.
func (c *Client) CellarReject(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/cellar/pending/"+url.PathEscape(id), nil, nil)
}

// CellarRemove removes a member.
func (c *Client) CellarRemove(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/cellar/members/"+url.PathEscape(id), nil, nil)
}

// CellarLeave leaves the cellar.
func (c *Client) CellarLeave(ctx context.Context) error {
	return c.do(ctx, "POST", "/cellar/leave", nil, nil)
}

// CellarForgetApproval removes a consumed approval record.
func (c *Client) CellarForgetApproval(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/cellar/approvals/"+url.PathEscape(id), nil, nil)
}

// Transfers lists import/export jobs.
func (c *Client) Transfers(ctx context.Context) ([]transfer.Job, error) {
	var v []transfer.Job
	return v, c.do(ctx, "GET", "/transfers", nil, &v)
}

// StartTransfer launches a job.
func (c *Client) StartTransfer(ctx context.Context, req admin.TransferRequest) (*transfer.Job, error) {
	var v transfer.Job
	return &v, c.do(ctx, "POST", "/transfers", req, &v)
}

// GetTransfer fetches one job.
func (c *Client) GetTransfer(ctx context.Context, id string) (*transfer.Job, error) {
	var v transfer.Job
	return &v, c.do(ctx, "GET", "/transfers/"+url.PathEscape(id), nil, &v)
}

// CancelTransfer cancels a running job or removes a finished one.
func (c *Client) CancelTransfer(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/transfers/"+url.PathEscape(id), nil, nil)
}

// GlobalReplication lists the server-wide targets (secrets masked).
func (c *Client) GlobalReplication(ctx context.Context) ([]replication.Target, error) {
	var v []replication.Target
	return v, c.do(ctx, "GET", "/replication/global", nil, &v)
}

// SetGlobalReplication adds or updates a server-wide target.
func (c *Client) SetGlobalReplication(ctx context.Context, target string, req admin.ReplicationRequest) ([]replication.Target, error) {
	var v []replication.Target
	q := ""
	if target != "" {
		q = "?target=" + url.QueryEscape(target)
	}
	return v, c.do(ctx, "PUT", "/replication/global"+q, req, &v)
}

// DeleteGlobalReplication removes one server-wide target, or all when empty.
func (c *Client) DeleteGlobalReplication(ctx context.Context, target string) error {
	q := ""
	if target != "" {
		q = "?target=" + url.QueryEscape(target)
	}
	return c.do(ctx, "DELETE", "/replication/global"+q, nil, nil)
}

// DeleteReplication removes one target, or all when target is empty.
func (c *Client) DeleteReplication(ctx context.Context, bucket, target string) error {
	q := ""
	if target != "" {
		q = "?target=" + url.QueryEscape(target)
	}
	return c.do(ctx, "DELETE", "/buckets/"+url.PathEscape(bucket)+"/replication"+q, nil, nil)
}
