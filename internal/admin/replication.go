package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/replication"
)

// ReplicationRequest adds or updates one standby target of a bucket. Fields
// left empty keep their current value when the target already exists.
type ReplicationRequest struct {
	Name      string  `json:"name,omitempty"` // defaults to the endpoint host
	Endpoint  string  `json:"endpoint,omitempty"`
	Bucket    string  `json:"bucket,omitempty"`
	AccessKey string  `json:"access_key,omitempty"`
	SecretKey string  `json:"secret_key,omitempty"`
	Region    string  `json:"region,omitempty"`
	PathStyle *bool   `json:"path_style,omitempty"`
	Prefix    *string `json:"prefix,omitempty"`
	Enabled   *bool   `json:"enabled,omitempty"`
	// SyncConfig (server-wide targets) also mirrors users, keys and settings.
	SyncConfig *bool `json:"sync_config,omitempty"`
}

func (h *Handler) replicationRoutes() []Route {
	return []Route{
		{"GET", "/replication", "Every replication target with lag and last error", true, h.listReplication},
		{"GET", "/buckets/{name}/replication", "A bucket's replication targets and status", true, h.getReplication},
		{"PUT", "/buckets/{name}/replication", "Add or update a standby target (?target=name, default: endpoint host)", true, h.putReplication},
		{"DELETE", "/buckets/{name}/replication", "Remove one target (?target=name) or all of a bucket's targets", true, h.deleteReplication},
		{"GET", "/replication/global", "Server-wide targets that replicate every bucket", true, h.getGlobalReplication},
		{"PUT", "/replication/global", "Add or update a server-wide target (?target=name, default: endpoint host)", true, h.putGlobalReplication},
		{"DELETE", "/replication/global", "Remove one server-wide target (?target=name) or all of them", true, h.deleteGlobalReplication},
	}
}

// maskTargets hides secrets before a rule leaves the server.
func maskTargets(ts []replication.Target) []replication.Target {
	out := make([]replication.Target, 0, len(ts))
	for _, t := range ts {
		t.SecretKey = "***"
		out = append(out, t)
	}
	return out
}

// applyRequest merges a request onto an existing target or a fresh one.
func applyRequest(t replication.Target, req ReplicationRequest) replication.Target {
	if req.Endpoint != "" {
		t.Endpoint = req.Endpoint
	}
	if req.Bucket != "" {
		t.Bucket = req.Bucket
	}
	if req.AccessKey != "" {
		t.AccessKey = req.AccessKey
	}
	if req.SecretKey != "" {
		t.SecretKey = req.SecretKey
	}
	if req.Region != "" {
		t.Region = req.Region
	}
	if req.PathStyle != nil {
		t.PathStyle = *req.PathStyle
	}
	if req.Prefix != nil {
		t.Prefix = *req.Prefix
	}
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	}
	if req.SyncConfig != nil {
		t.SyncConfig = *req.SyncConfig
	}
	t.Inherited = false
	return t
}

// upsertTarget adds or updates the named target in a rule.
func upsertTarget(rule replication.Rule, name string, req ReplicationRequest) replication.Rule {
	idx, exists := rule.Find(name)
	t := replication.Target{Name: name, Enabled: true}
	if exists {
		t = rule.Targets[idx]
	}
	t = applyRequest(t, req)
	targets := append([]replication.Target(nil), rule.Targets...)
	if exists {
		targets[idx] = t
	} else {
		targets = append(targets, t)
	}
	return replication.Rule{Targets: targets}
}

// splitTargets separates the targets to remove (all when name is empty).
func splitTargets(rule replication.Rule, name string) (removed []string, remaining []replication.Target) {
	for _, t := range rule.Targets {
		if name == "" || t.Name == name {
			removed = append(removed, t.Name)
			continue
		}
		remaining = append(remaining, t)
	}
	return removed, remaining
}

func (h *Handler) resolveTargetName(c *Call, req ReplicationRequest) (string, error) {
	query := c.R.URL.Query().Get("target")
	if query != "" && req.Name != "" && query != req.Name {
		return "", Errorf(http.StatusBadRequest, "InvalidArgument", "target name in query (%q) and body (%q) differ", query, req.Name)
	}
	name := strings.TrimSpace(targetName(req, query))
	if name == "" {
		return "", Errorf(http.StatusBadRequest, "InvalidArgument", "a target name or an endpoint is required")
	}
	return name, nil
}

func (h *Handler) getGlobalReplication(c *Call) error {
	rule, _, err := replication.LoadGlobal(c.Ctx(), h.d.DB)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, maskTargets(rule.Targets))
}

func (h *Handler) putGlobalReplication(c *Call) error {
	var req ReplicationRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	name, err := h.resolveTargetName(c, req)
	if err != nil {
		return err
	}
	req.Bucket = "" // the destination bucket always mirrors the source name
	if err := h.protectEndpoint(c, req.Endpoint); err != nil {
		return err
	}
	h.replMu.Lock()
	defer h.replMu.Unlock()
	rule, _, _ := replication.LoadGlobal(c.Ctx(), h.d.DB)
	rule = upsertTarget(rule, name, req)
	if err := rule.ValidateGlobal(); err != nil {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "%v", err)
	}
	raw, _ := json.Marshal(rule)
	if _, err := h.propose(c, &meta.SetSetting{Key: replication.GlobalKey, Value: string(raw)}); err != nil {
		return err
	}
	idx, _ := rule.Find(name)
	h.audit(c, "replication.global.set", name, rule.Targets[idx].Endpoint)
	if h.d.Replication != nil {
		h.d.Replication.Trigger()
	}
	return c.JSON(http.StatusOK, maskTargets(rule.Targets))
}

func (h *Handler) deleteGlobalReplication(c *Call) error {
	h.replMu.Lock()
	defer h.replMu.Unlock()
	rule, exists, _ := replication.LoadGlobal(c.Ctx(), h.d.DB)
	if !exists {
		return Errorf(http.StatusNotFound, "NoReplicationRule", "no server-wide replication targets")
	}
	name := c.R.URL.Query().Get("target")
	removed, remaining := splitTargets(rule, name)
	if len(removed) == 0 {
		return Errorf(http.StatusNotFound, "NoReplicationRule", "no server-wide target %q", name)
	}
	value := ""
	if len(remaining) > 0 {
		raw, _ := json.Marshal(replication.Rule{Targets: remaining})
		value = string(raw)
	}
	if _, err := h.propose(c, &meta.SetSetting{Key: replication.GlobalKey, Value: value}); err != nil {
		return err
	}
	if err := h.dropInheritedCursors(c, removed); err != nil {
		return err
	}
	h.audit(c, "replication.global.delete", strings.Join(removed, ","), "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

// dropInheritedCursors removes the per-bucket cursors of removed server-wide
// targets, except where a bucket has its own target of the same name.
func (h *Handler) dropInheritedCursors(c *Call, names []string) error {
	all, err := h.d.DB.AllSettings(c.Ctx())
	if err != nil {
		return err
	}
	gone := map[string]bool{}
	for _, n := range names {
		gone[n] = true
	}
	for key := range all {
		if !strings.HasPrefix(key, replication.CursorPrefix) {
			continue
		}
		rest := strings.TrimPrefix(key, replication.CursorPrefix)
		bucket, name, ok := strings.Cut(rest, ":")
		if !ok || !gone[name] {
			continue
		}
		if own, _, _ := replication.LoadRule(c.Ctx(), h.d.DB, bucket); own.Targets != nil {
			if _, shadowed := own.Find(name); shadowed {
				continue
			}
		}
		if _, err := h.propose(c, &meta.SetSetting{Key: key}); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) replicationStatus(c *Call) []replication.TargetStatus {
	if h.d.Replication == nil {
		return nil
	}
	return h.d.Replication.Status(c.Ctx())
}

func (h *Handler) listReplication(c *Call) error {
	out := h.replicationStatus(c)
	if out == nil {
		out = []replication.TargetStatus{}
	}
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) bucketTargets(c *Call, bucket string) []replication.TargetStatus {
	out := []replication.TargetStatus{}
	for _, st := range h.replicationStatus(c) {
		if st.Bucket == bucket {
			out = append(out, st)
		}
	}
	return out
}

func (h *Handler) getReplication(c *Call) error {
	b, err := h.bucketByName(c)
	if err != nil {
		return err
	}
	out := h.bucketTargets(c, b.Name)
	if len(out) == 0 {
		return Errorf(http.StatusNotFound, "NoReplicationRule", "bucket %q has no replication targets", b.Name)
	}
	return c.JSON(http.StatusOK, out)
}

func targetName(req ReplicationRequest, query string) string {
	if query != "" {
		return query
	}
	if req.Name != "" {
		return req.Name
	}
	if u, err := url.Parse(req.Endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return ""
}

func (h *Handler) putReplication(c *Call) error {
	b, err := h.bucketByName(c)
	if err != nil {
		return err
	}
	var req ReplicationRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	name, err := h.resolveTargetName(c, req)
	if err != nil {
		return err
	}
	if req.SyncConfig != nil && *req.SyncConfig {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "sync_config applies to server-wide targets only")
	}
	if err := h.protectEndpoint(c, req.Endpoint); err != nil {
		return err
	}
	h.replMu.Lock()
	defer h.replMu.Unlock()
	rule, _, _ := replication.LoadRule(c.Ctx(), h.d.DB, b.Name)
	if _, exists := rule.Find(name); !exists && req.Bucket == "" {
		req.Bucket = b.Name // a new target mirrors to a bucket of the same name by default
	}
	rule = upsertTarget(rule, name, req)
	if err := rule.Validate(); err != nil {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "%v", err)
	}
	raw, _ := json.Marshal(rule)
	if _, err := h.propose(c, &meta.SetSetting{Key: replication.RulePrefix + b.Name, Value: string(raw)}); err != nil {
		return err
	}
	idx, _ := rule.Find(name)
	t := rule.Targets[idx]
	h.audit(c, "replication.set", b.Name, name+" -> "+t.Endpoint+"/"+t.Bucket)
	if h.d.Replication != nil {
		h.d.Replication.Trigger()
	}
	return c.JSON(http.StatusOK, h.bucketTargets(c, b.Name))
}

func (h *Handler) deleteReplication(c *Call) error {
	b, err := h.bucketByName(c)
	if err != nil {
		return err
	}
	h.replMu.Lock()
	defer h.replMu.Unlock()
	rule, exists, _ := replication.LoadRule(c.Ctx(), h.d.DB, b.Name)
	if !exists {
		return Errorf(http.StatusNotFound, "NoReplicationRule", "bucket %q has no replication targets", b.Name)
	}
	name := c.R.URL.Query().Get("target")
	removed, remaining := splitTargets(rule, name)
	if len(removed) == 0 {
		if global, _, _ := replication.LoadGlobal(c.Ctx(), h.d.DB); global.Targets != nil {
			if _, inherited := global.Find(name); inherited {
				return Errorf(http.StatusConflict, "InheritedTarget", "target %q is server-wide; remove it with DELETE /replication/global?target=%s", name, url.QueryEscape(name))
			}
		}
		return Errorf(http.StatusNotFound, "NoReplicationRule", "bucket %q has no target %q", b.Name, name)
	}
	value := ""
	if len(remaining) > 0 {
		raw, _ := json.Marshal(replication.Rule{Targets: remaining})
		value = string(raw)
	}
	if _, err := h.propose(c, &meta.SetSetting{Key: replication.RulePrefix + b.Name, Value: value}); err != nil {
		return err
	}
	for _, n := range removed {
		if _, err := h.propose(c, &meta.SetSetting{Key: replication.CursorKey(b.Name, n)}); err != nil {
			return err
		}
	}
	h.audit(c, "replication.delete", b.Name, strings.Join(removed, ","))
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}
