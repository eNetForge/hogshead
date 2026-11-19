// Package cellar groups hogshead servers into a cellar: a set of peers that
// hold a full copy of each other's buckets, users and settings. It is built
// on replication (mesh targets with config sync and write acknowledgement)
// and adds only membership: init, approved joins, removal, leaving, and a
// catch-up readiness signal. There is no leader and no consensus; the member
// list is an ordinary replicated setting.
package cellar

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/replication"
	"github.com/enetforge/hogshead/internal/s3client"
)

// Settings. Everything under cellar.* replicates to members through config
// sync, so a change made on one member reaches all of them; the targets
// derived from the member list stay local to each server.
const (
	KeyID       = "cellar.id"
	KeyName     = "cellar.name"
	KeyMembers  = "cellar.members"
	KeyAccessID = "cellar.key" // access key id of the service account
	KeyWriteAck = "cellar.write_ack"
	PendingPfx  = "cellar.pending:"
	ApprovedPfx = "cellar.approved:"
	// ServiceUser is the administrator account the members use for each other.
	ServiceUser = "cellar"
	// TargetPrefix names the replication targets the cellar manages.
	TargetPrefix = "cellar:"
	// Local (never replicated) state.
	keyJoin       = "replication.cellar_join"
	backfilledPfx = "replication.cellar_backfilled:"
	// PublicPrefix is where the unauthenticated join endpoints live.
	PublicPrefix = "/_cellar"
)

// Member is one server in the cellar.
type Member struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Endpoint string    `json:"endpoint"`
	Version  string    `json:"version,omitempty"`
	JoinedAt time.Time `json:"joined_at"`
}

// JoinRequest is a pending request from a server that wants in.
type JoinRequest struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Endpoint    string    `json:"endpoint"`
	Version     string    `json:"version,omitempty"`
	NonceHash   string    `json:"nonce_hash"`
	RequestedAt time.Time `json:"requested_at"`
	// Buckets is how many buckets the joiner brings (a merge); 0 for an empty server.
	Buckets int `json:"buckets"`
}

// approval is written when an admin approves a request; the joiner collects
// it with its nonce.
type approval struct {
	ID         string    `json:"id"`
	NonceHash  string    `json:"nonce_hash"`
	ApprovedAt time.Time `json:"approved_at"`
	By         string    `json:"by"`
}

// joinState is the joiner's local record of an outstanding request.
type joinState struct {
	Address     string    `json:"address"` // member we asked
	Nonce       string    `json:"nonce"`
	Endpoint    string    `json:"endpoint"` // what we advertised
	RequestedAt time.Time `json:"requested_at"`
	LastError   string    `json:"last_error,omitempty"`
	Merge       bool      `json:"merge,omitempty"`
}

// MemberStatus is a member as reported by the admin API.
type MemberStatus struct {
	Member
	Self        bool   `json:"self"`
	Enabled     bool   `json:"enabled"`
	Lag         int64  `json:"lag"` // changes not yet pushed from here to it
	LastError   string `json:"last_error,omitempty"`
	Backfilling bool   `json:"backfilling"`
	Backfilled  bool   `json:"backfilled"`
	// Joining is true between approval and the moment the server has
	// collected its credentials; no target exists yet and no error can.
	Joining bool `json:"joining"`
}

// Status is the admin API view of the cellar.
type Status struct {
	Active     bool           `json:"active"`
	ID         string         `json:"id,omitempty"`
	Name       string         `json:"name,omitempty"`
	WriteAck   string         `json:"write_ack,omitempty"`
	SelfID     string         `json:"self_id"`
	Members    []MemberStatus `json:"members"`
	Pending    []JoinRequest  `json:"pending"`
	Ready      bool           `json:"ready"`
	Backlog    int64          `json:"backlog"` // changes other members still have to push to us
	Joining    *joinState     `json:"joining,omitempty"`
	LastCheck  time.Time      `json:"last_check,omitempty"`
	CheckError string         `json:"check_error,omitempty"`
	// Notices are recent things the cellar did on its own, such as pausing a
	// manual replication target that pointed at a member.
	Notices []Notice `json:"notices"`
}

// Notice is a message with a time.
type Notice struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// Deps are the manager's collaborators.
type Deps struct {
	DB       *meta.DB
	Propose  func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	Worker   *replication.Worker
	ServerID func() string
	Name     func() string
	// Endpoint is how other servers reach this one (scheme://host:port).
	Endpoint func() string
	Version  string
	Log      *slog.Logger
	Now      func() time.Time
	// HTTP performs the joiner's calls to a member; nil uses a default client.
	HTTP *http.Client
}

// Manager runs the membership logic.
type Manager struct {
	d    Deps
	mu   sync.Mutex
	kick chan struct{}

	backfilling map[string]bool
	notices     []Notice
	ready       bool
	backlog     int64
	lastCheck   time.Time
	checkErr    string
	zeroStreak  int
}

// New builds a manager.
func New(d Deps) *Manager {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	return &Manager{d: d, kick: make(chan struct{}, 1), backfilling: map[string]bool{}, ready: true}
}

// Trigger asks for a reconcile soon.
func (m *Manager) Trigger() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run reconciles targets, polls an outstanding join and checks the catch-up
// backlog until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		m.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
	}
}

// Step performs one iteration of the loop: reconcile targets, poll an
// outstanding join request and refresh the catch-up backlog.
func (m *Manager) Step(ctx context.Context) {
	if err := m.refreshSelf(ctx); err != nil && ctx.Err() == nil {
		m.d.Log.Warn("cellar: refreshing this server's member record failed", "err", err)
	}
	if err := m.Reconcile(ctx); err != nil && ctx.Err() == nil {
		m.d.Log.Warn("cellar: reconcile failed", "err", err)
	}
	if err := m.pollJoin(ctx); err != nil && ctx.Err() == nil {
		m.d.Log.Warn("cellar: join poll failed", "err", err)
	}
	m.checkBacklog(ctx)
}

// refreshSelf keeps this server's entry in the member list current after an
// upgrade: the version recorded at join time would otherwise be shown for
// ever. The list is a replicated setting, so every member sees the change.
func (m *Manager) refreshSelf(ctx context.Context) error {
	if m.d.Version == "" || !m.Active(ctx) {
		return nil
	}
	members, err := m.Members(ctx)
	if err != nil {
		return fmt.Errorf("load members: %w", err)
	}
	updated := make([]Member, len(members))
	changed := false
	for i, mem := range members {
		if mem.ID == m.d.ServerID() && mem.Version != m.d.Version {
			mem.Version = m.d.Version
			changed = true
		}
		updated[i] = mem
	}
	if !changed {
		return nil
	}
	if err := m.saveMembers(ctx, updated); err != nil {
		return fmt.Errorf("save members: %w", err)
	}
	return nil
}

// ---- notices ----

func (m *Manager) notify(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notices = append(m.notices, Notice{At: m.d.Now(), Message: msg})
	if len(m.notices) > 20 {
		m.notices = m.notices[len(m.notices)-20:]
	}
	m.d.Log.Info("cellar: " + msg)
}

// ---- overlap with manual replication ----

// hostOf normalises an endpoint to host:port for comparison.
func hostOf(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(endpoint))
	}
	host := strings.ToLower(u.Host)
	if u.Port() == "" {
		port := "9000"
		if u.Scheme == "https" {
			port = "443"
		} else if u.Scheme == "http" && strings.Contains(endpoint, ":80/") {
			port = "80"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	return host
}

// Peer is another member with a client authenticated as the cellar.
type Peer struct {
	Member
	Client *s3client.Client
}

// Peers returns clients for every other member, for fan-out queries such
// as cellar-wide usage. Nil when this server is not in a cellar.
func (m *Manager) Peers(ctx context.Context) ([]Peer, error) {
	if !m.Active(ctx) {
		return nil, nil
	}
	members, err := m.Members(ctx)
	if err != nil {
		return nil, err
	}
	key, err := m.serviceKey(ctx)
	if err != nil {
		return nil, err
	}
	var out []Peer
	for _, mem := range members {
		if mem.ID == m.d.ServerID() {
			continue
		}
		ep := key
		ep.Endpoint = mem.Endpoint
		out = append(out, Peer{Member: mem, Client: s3client.New(ep)})
	}
	return out, nil
}

// MemberFor returns the member whose endpoint matches, if any (never self).
func (m *Manager) MemberFor(ctx context.Context, endpoint string) *Member {
	if !m.Active(ctx) {
		return nil
	}
	members, _ := m.Members(ctx)
	h := hostOf(endpoint)
	for i := range members {
		if members[i].ID != m.d.ServerID() && hostOf(members[i].Endpoint) == h {
			return &members[i]
		}
	}
	return nil
}

// manualTargets lists the non-cellar replication targets of this server:
// server-wide ones and per-bucket ones, with the setting key they live in.
type manualTarget struct {
	key    string // setting key
	bucket string // "" for server-wide
	t      replication.Target
}

func (m *Manager) manualTargets(ctx context.Context) ([]manualTarget, error) {
	var out []manualTarget
	global, _, err := replication.LoadGlobal(ctx, m.d.DB)
	if err != nil {
		return nil, err
	}
	for _, t := range global.Targets {
		if !t.Cellar {
			out = append(out, manualTarget{key: replication.GlobalKey, t: t})
		}
	}
	all, err := m.d.DB.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	for k, v := range all {
		if !strings.HasPrefix(k, replication.RulePrefix) {
			continue
		}
		var r replication.Rule
		if json.Unmarshal([]byte(v), &r) != nil {
			continue
		}
		for _, t := range r.Targets {
			out = append(out, manualTarget{key: k, bucket: strings.TrimPrefix(k, replication.RulePrefix), t: t})
		}
	}
	return out, nil
}

// overlapping returns manual targets whose endpoint matches one of hosts.
func (m *Manager) overlapping(ctx context.Context, hosts map[string]string) ([]manualTarget, error) {
	targets, err := m.manualTargets(ctx)
	if err != nil {
		return nil, err
	}
	var out []manualTarget
	for _, mt := range targets {
		if _, ok := hosts[hostOf(mt.t.Endpoint)]; ok {
			out = append(out, mt)
		}
	}
	return out, nil
}

func describeTarget(mt manualTarget) string {
	if mt.bucket == "" {
		return fmt.Sprintf("server-wide target %q -> %s", mt.t.Name, mt.t.Endpoint)
	}
	return fmt.Sprintf("bucket %s target %q -> %s", mt.bucket, mt.t.Name, mt.t.Endpoint)
}

// pauseOverlaps disables manual targets that point at cellar members: the
// cellar replicates between members itself, and a second path would push
// everything twice. The targets are kept, paused, so nothing is lost.
func (m *Manager) pauseOverlaps(ctx context.Context) error {
	members, err := m.Members(ctx)
	if err != nil {
		return err
	}
	hosts := map[string]string{}
	for _, mem := range members {
		if mem.ID != m.d.ServerID() {
			hosts[hostOf(mem.Endpoint)] = mem.Name
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	over, err := m.overlapping(ctx, hosts)
	if err != nil {
		return err
	}
	byKey := map[string][]manualTarget{}
	for _, mt := range over {
		if mt.t.Enabled {
			byKey[mt.key] = append(byKey[mt.key], mt)
		}
	}
	for key, list := range byKey {
		raw := m.setting(ctx, key)
		var rule replication.Rule
		if json.Unmarshal([]byte(raw), &rule) != nil {
			continue
		}
		for i := range rule.Targets {
			for _, mt := range list {
				if rule.Targets[i].Name == mt.t.Name && !rule.Targets[i].Cellar {
					rule.Targets[i].Enabled = false
					m.notify(fmt.Sprintf("paused %s: %s is a cellar member and the cellar replicates to it already", describeTarget(mt), hosts[hostOf(mt.t.Endpoint)]))
				}
			}
		}
		if err := m.setJSON(ctx, key, rule); err != nil {
			return err
		}
	}
	return nil
}

// ---- state helpers ----

func (m *Manager) setting(ctx context.Context, key string) string {
	v, _ := m.d.DB.GetSetting(ctx, key)
	return v
}

func (m *Manager) set(ctx context.Context, key, value string) error {
	res, err := m.d.Propose(ctx, &meta.SetSetting{Key: key, Value: value})
	if err != nil {
		return err
	}
	return res.Error()
}

func (m *Manager) setJSON(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.set(ctx, key, string(raw))
}

// Active reports whether this server is in a cellar.
func (m *Manager) Active(ctx context.Context) bool { return m.setting(ctx, KeyID) != "" }

// Members returns the member list.
func (m *Manager) Members(ctx context.Context) ([]Member, error) {
	raw := m.setting(ctx, KeyMembers)
	if raw == "" {
		return nil, nil
	}
	var out []Member
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) saveMembers(ctx context.Context, members []Member) error {
	return m.setJSON(ctx, KeyMembers, members)
}

// serviceKey returns the cellar service account credentials held locally.
func (m *Manager) serviceKey(ctx context.Context) (s3client.Endpoint, error) {
	ak := m.setting(ctx, KeyAccessID)
	if ak == "" {
		return s3client.Endpoint{}, errors.New("cellar service key not set")
	}
	cred, err := m.d.DB.LookupAccessKey(ctx, ak)
	if err != nil {
		return s3client.Endpoint{}, fmt.Errorf("cellar service key: %w", err)
	}
	return s3client.Endpoint{AccessKey: cred.Key.AccessKeyID, SecretKey: cred.Key.Secret, PathStyle: true}, nil
}

// ensureServiceAccount creates the admin user and key the members use, or
// imports the given ones on a joiner.
func (m *Manager) ensureServiceAccount(ctx context.Context, ak, sk string) error {
	u, err := m.d.DB.GetUserByName(ctx, ServiceUser)
	if errors.Is(err, meta.ErrNoSuchUser) {
		uid := ids.UserID()
		if _, err := m.d.Propose(ctx, &meta.CreateUser{ID: uid, Name: ServiceUser, Role: meta.RoleAdmin, Now: m.d.Now().UnixMilli()}); err != nil {
			return err
		}
		u, err = m.d.DB.GetUser(ctx, uid)
	}
	if err != nil {
		return err
	}
	// A pre-existing account of that name (from before the name was reserved)
	// becomes a clean service account: administrator, unscoped, enabled, no
	// web UI password.
	role, scope, disabled := meta.RoleAdmin, []string{}, false
	if u.Role != role || len(u.Buckets) != 0 || u.Disabled {
		if _, err := m.d.Propose(ctx, &meta.UpdateUser{ID: u.ID, Role: &role, Buckets: &scope, Disabled: &disabled}); err != nil {
			return err
		}
	}
	if u.PasswordHash != "" {
		if _, err := m.d.Propose(ctx, &meta.SetUserPassword{ID: u.ID, Hash: ""}); err != nil {
			return err
		}
	}
	if ak == "" {
		ak, sk = ids.AccessKeyID(), ids.SecretKey()
	}
	if cur, err := m.d.DB.LookupAccessKey(ctx, ak); err == nil && cur.Key.Secret == sk && cur.User.ID == u.ID {
		return m.set(ctx, KeyAccessID, ak)
	} else if err == nil {
		if _, err := m.d.Propose(ctx, &meta.DeleteAccessKey{AccessKeyID: ak}); err != nil {
			return err
		}
	}
	if _, err := m.d.Propose(ctx, &meta.CreateAccessKey{AccessKeyID: ak, Secret: sk, UserID: u.ID, Label: "cellar", Now: m.d.Now().UnixMilli()}); err != nil {
		return err
	}
	return m.set(ctx, KeyAccessID, ak)
}

// ---- init / leave / remove ----

// Init creates a new cellar with this server as its first member.
func (m *Manager) Init(ctx context.Context, name, writeAck string) (*Status, error) {
	if m.Active(ctx) {
		return nil, errors.New("this server is already in a cellar")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("a cellar name is required")
	}
	if writeAck == "" {
		writeAck = replication.AckOne
	}
	if err := validAck(writeAck); err != nil {
		return nil, err
	}
	if err := m.ensureServiceAccount(ctx, "", ""); err != nil {
		return nil, err
	}
	self := Member{ID: m.d.ServerID(), Name: m.d.Name(), Endpoint: m.d.Endpoint(), Version: m.d.Version, JoinedAt: m.d.Now()}
	if err := m.saveMembers(ctx, []Member{self}); err != nil {
		return nil, err
	}
	for k, v := range map[string]string{KeyID: "c-" + hex.EncodeToString(randomBytes(8)), KeyName: strings.TrimSpace(name), KeyWriteAck: writeAck} {
		if err := m.set(ctx, k, v); err != nil {
			return nil, err
		}
	}
	m.Trigger()
	return m.Status(ctx)
}

func validAck(v string) error {
	switch v {
	case replication.AckLocal, replication.AckOne, replication.AckAll:
		return nil
	}
	return errors.New("write_ack must be local, one or all")
}

// Update changes the cellar name or write acknowledgement level.
func (m *Manager) Update(ctx context.Context, name, writeAck string) (*Status, error) {
	if !m.Active(ctx) {
		return nil, ErrNotInCellar
	}
	if name != "" {
		if err := m.set(ctx, KeyName, strings.TrimSpace(name)); err != nil {
			return nil, err
		}
	}
	if writeAck != "" {
		if err := validAck(writeAck); err != nil {
			return nil, err
		}
		if err := m.set(ctx, KeyWriteAck, writeAck); err != nil {
			return nil, err
		}
	}
	return m.Status(ctx)
}

// ErrNotInCellar is returned for cellar operations on a standalone server.
var ErrNotInCellar = errors.New("this server is not in a cellar")

// Leave removes this server from the cellar: the member list shrinks (and
// replicates out before the targets go), then local cellar state is cleared.
func (m *Manager) Leave(ctx context.Context) error {
	if !m.Active(ctx) {
		return ErrNotInCellar
	}
	members, err := m.Members(ctx)
	if err != nil {
		return err
	}
	var rest []Member
	for _, mem := range members {
		if mem.ID != m.d.ServerID() {
			rest = append(rest, mem)
		}
	}
	if err := m.saveMembers(ctx, rest); err != nil {
		return err
	}
	// Give the shrunken list a moment to replicate before dropping targets.
	if len(rest) > 0 {
		_ = m.d.Worker.Pass(ctx)
	}
	for _, k := range []string{KeyID, KeyName, KeyMembers, KeyAccessID, KeyWriteAck, keyJoin} {
		if err := m.set(ctx, k, ""); err != nil {
			return err
		}
	}
	all, _ := m.d.DB.AllSettings(ctx)
	for k := range all {
		if strings.HasPrefix(k, PendingPfx) || strings.HasPrefix(k, ApprovedPfx) || strings.HasPrefix(k, backfilledPfx) {
			_ = m.set(ctx, k, "")
		}
	}
	return m.Reconcile(ctx)
}

// Remove drops another member and rotates the service key so it loses access.
func (m *Manager) Remove(ctx context.Context, id string) error {
	if !m.Active(ctx) {
		return ErrNotInCellar
	}
	if id == m.d.ServerID() {
		return errors.New("use leave to remove this server")
	}
	members, err := m.Members(ctx)
	if err != nil {
		return err
	}
	var rest []Member
	found := false
	for _, mem := range members {
		if mem.ID == id {
			found = true
			continue
		}
		rest = append(rest, mem)
	}
	if !found {
		return fmt.Errorf("no member %q", id)
	}
	// Order matters. 1) Drop the member from the list and from our targets,
	// so nothing below reaches it. 2) Push the new list to the others with
	// the current key. 3) Mint the new key and push it, still signed with
	// the old key, so every remaining member has it before we switch.
	// 4) Switch our targets to the new key and retire the old one; that
	// deletion reaches the others signed with the new key.
	if err := m.saveMembers(ctx, rest); err != nil {
		return err
	}
	_ = m.set(ctx, backfilledPfx+id, "")
	if err := m.Reconcile(ctx); err != nil {
		return err
	}
	_ = m.d.Worker.Pass(ctx)
	old := m.setting(ctx, KeyAccessID)
	u, err := m.d.DB.GetUserByName(ctx, ServiceUser)
	if err != nil {
		return err
	}
	ak, sk := ids.AccessKeyID(), ids.SecretKey()
	if _, err := m.d.Propose(ctx, &meta.CreateAccessKey{AccessKeyID: ak, Secret: sk, UserID: u.ID, Label: "cellar", Now: m.d.Now().UnixMilli()}); err != nil {
		return err
	}
	if err := m.set(ctx, KeyAccessID, ak); err != nil {
		return err
	}
	_ = m.d.Worker.Pass(ctx) // new key and setting travel under the old key
	if err := m.Reconcile(ctx); err != nil {
		return err
	}
	if old != "" {
		if _, err := m.d.Propose(ctx, &meta.DeleteAccessKey{AccessKeyID: old}); err != nil && !errors.Is(err, meta.ErrNoSuchKeyID) {
			return err
		}
	}
	m.d.Worker.Trigger()
	return nil
}

// ---- reconcile: member list -> replication targets ----

// Reconcile makes the server-wide replication rule hold exactly one cellar
// target per other member, leaving non-cellar targets alone, and starts a
// backfill towards members that have not had one.
func (m *Manager) Reconcile(ctx context.Context) error {
	global, _, err := replication.LoadGlobal(ctx, m.d.DB)
	if err != nil {
		return err
	}
	var keep []replication.Target
	for _, t := range global.Targets {
		if !t.Cellar {
			keep = append(keep, t)
		}
	}
	var desired []replication.Target
	if m.Active(ctx) {
		members, err := m.Members(ctx)
		if err != nil {
			return err
		}
		key, err := m.serviceKey(ctx)
		if err != nil {
			return err
		}
		for _, mem := range members {
			if mem.ID == m.d.ServerID() || m.stillJoining(ctx, mem.ID) {
				continue
			}
			desired = append(desired, replication.Target{
				Name: TargetPrefix + mem.ID, Enabled: true, Endpoint: mem.Endpoint,
				AccessKey: key.AccessKey, SecretKey: key.SecretKey, PathStyle: true,
				SyncConfig: true, Cellar: true,
			})
		}
	}
	// Preserve the enabled flag of targets an admin paused.
	for i := range desired {
		for _, old := range global.Targets {
			if old.Cellar && old.Name == desired[i].Name {
				desired[i].Enabled = old.Enabled
			}
		}
	}
	next := replication.Rule{Targets: append(keep, desired...)}
	if !sameTargets(global.Targets, next.Targets) {
		// A target that did not exist before starts at the log head: history
		// reaches the member through the backfill, not through replay.
		existing := map[string]bool{}
		for _, t := range global.Targets {
			existing[t.Name] = true
		}
		for _, t := range desired {
			if !existing[t.Name] {
				if err := m.d.Worker.SkipBacklog(ctx, t.Name); err != nil {
					return err
				}
			}
		}
		if err := m.setJSON(ctx, replication.GlobalKey, next); err != nil {
			return err
		}
		m.d.Worker.Trigger()
	}
	if len(desired) > 0 {
		if err := m.pauseOverlaps(ctx); err != nil {
			return err
		}
		members, _ := m.Members(ctx)
		joined := map[string]time.Time{}
		for _, mem := range members {
			joined[mem.ID] = mem.JoinedAt
		}
		for _, t := range desired {
			id := strings.TrimPrefix(t.Name, TargetPrefix)
			if m.setting(ctx, backfilledPfx+id) == "" {
				// The longer-standing member's configuration wins a clash.
				configWins := joined[m.d.ServerID()].Before(joined[id])
				m.startBackfill(t, id, configWins)
			}
		}
	}
	return nil
}

// joinGrace bounds how long an uncollected approval delays the target; a
// joiner polls every few seconds, so this only matters if it never does.
const joinGrace = 2 * time.Minute

// stillJoining reports whether an approved server has not yet collected its
// credentials: its approval record still exists and is recent. Pushing to it
// before that would be refused as "not in cellar".
func (m *Manager) stillJoining(ctx context.Context, id string) bool {
	raw := m.setting(ctx, ApprovedPfx+id)
	if raw == "" {
		return false
	}
	var ap approval
	if json.Unmarshal([]byte(raw), &ap) != nil {
		return false
	}
	return m.d.Now().Sub(ap.ApprovedAt) < joinGrace
}

func sameTargets(a, b []replication.Target) bool {
	if len(a) != len(b) {
		return false
	}
	ra, _ := json.Marshal(a) //nolint:gosec // compared in memory only, never logged or sent
	rb, _ := json.Marshal(b) //nolint:gosec // see above
	return string(ra) == string(rb)
}

func (m *Manager) startBackfill(t replication.Target, id string, configWins bool) {
	m.mu.Lock()
	if m.backfilling[id] {
		m.mu.Unlock()
		return
	}
	m.backfilling[id] = true
	m.mu.Unlock()
	// A backfill can take hours and must outlive the reconcile that started it.
	go func() { //nolint:gosec // G118: intentional, see above
		defer func() {
			m.mu.Lock()
			delete(m.backfilling, id)
			m.mu.Unlock()
		}()
		bctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		if err := m.d.Worker.Backfill(bctx, t, configWins, nil); err != nil {
			m.d.Log.Warn("cellar: backfill failed, will retry", "member", id, "err", err)
			return
		}
		if err := m.set(context.Background(), backfilledPfx+id, m.d.Now().UTC().Format(time.RFC3339)); err != nil {
			m.d.Log.Warn("cellar: record backfill", "member", id, "err", err)
		}
		m.d.Log.Info("cellar: backfill complete", "member", id)
	}()
}

// ---- join: joiner side ----

// ErrNotEmpty is returned when a server with buckets tries to join without merge.
type ErrNotEmpty struct{ Buckets []string }

func (e *ErrNotEmpty) Error() string {
	n := len(e.Buckets)
	list := strings.Join(e.Buckets, ", ")
	if n > 5 {
		list = strings.Join(e.Buckets[:5], ", ") + fmt.Sprintf(" and %d more", n-5)
	}
	return fmt.Sprintf("this server has %d bucket(s) (%s); a server joining a cellar must be empty. Export or delete them first, or merge them into the cellar explicitly", n, list)
}

// Join asks a member at address (host:port or URL) to let this server in and
// records the request so Run can poll for approval. A server that already
// has buckets is refused unless merge is set, in which case its buckets are
// added to the cellar (see completeJoin for the clash rules).
func (m *Manager) Join(ctx context.Context, address, advertise string, merge bool) (*Status, error) {
	if m.Active(ctx) {
		return nil, errors.New("this server is already in a cellar; leave it first")
	}
	buckets, err := m.d.DB.ListBuckets(ctx, "")
	if err != nil {
		return nil, err
	}
	if len(buckets) > 0 && !merge {
		e := &ErrNotEmpty{}
		for _, b := range buckets {
			e.Buckets = append(e.Buckets, b.Name)
		}
		return nil, e
	}
	base, err := normalizeAddress(address)
	if err != nil {
		return nil, err
	}
	over, err := m.overlapping(ctx, map[string]string{hostOf(base): base})
	if err != nil {
		return nil, err
	}
	if len(over) > 0 {
		var names []string
		for _, mt := range over {
			names = append(names, describeTarget(mt))
		}
		return nil, fmt.Errorf("this server already replicates to %s: %s. Remove those targets first; inside a cellar replication between members is managed by the cellar", base, strings.Join(names, "; "))
	}
	if advertise == "" {
		advertise = m.d.Endpoint()
	}
	nonce := hex.EncodeToString(randomBytes(16))
	req := map[string]any{"id": m.d.ServerID(), "name": m.d.Name(), "endpoint": advertise, "version": m.d.Version, "nonce": nonce, "buckets": len(buckets)}
	var out struct {
		Cellar string `json:"cellar"`
		Name   string `json:"name"`
	}
	if err := m.call(ctx, http.MethodPost, base+PublicPrefix+"/join", req, &out); err != nil {
		return nil, err
	}
	st := joinState{Address: base, Nonce: nonce, Endpoint: advertise, RequestedAt: m.d.Now(), Merge: merge}
	if err := m.setJSON(ctx, keyJoin, st); err != nil {
		return nil, err
	}
	m.Trigger()
	return m.Status(ctx)
}

// CancelJoin forgets an outstanding join request.
func (m *Manager) CancelJoin(ctx context.Context) error { return m.set(ctx, keyJoin, "") }

func normalizeAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", errors.New("an address is required")
	}
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid address %q", address)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "9000")
	}
	return u.Scheme + "://" + u.Host, nil
}

func (m *Manager) call(ctx context.Context, method, target string, in, out any) error {
	var body *strings.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(raw))
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf strings.Builder
	_, _ = io.Copy(&buf, resp.Body)
	if resp.StatusCode == http.StatusAccepted {
		return errPending
	}
	if resp.StatusCode >= 300 {
		return &callError{Status: resp.StatusCode, Body: strings.TrimSpace(buf.String())}
	}
	if out != nil && buf.Len() > 0 {
		return json.Unmarshal([]byte(buf.String()), out)
	}
	return nil
}

var errPending = errors.New("pending approval")

type callError struct {
	Status int
	Body   string
}

func (e *callError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Body) }

// welcome is what an approved joiner receives.
type welcome struct {
	Cellar   string   `json:"cellar"`
	Name     string   `json:"name"`
	Members  []Member `json:"members"`
	Key      string   `json:"key"`
	Secret   string   `json:"secret"`
	WriteAck string   `json:"write_ack"`
}

// pollJoin asks the member we applied to whether we were approved and, if
// so, completes the join.
func (m *Manager) pollJoin(ctx context.Context) error {
	raw := m.setting(ctx, keyJoin)
	if raw == "" || m.Active(ctx) {
		return nil
	}
	var st joinState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return m.set(ctx, keyJoin, "")
	}
	var w welcome
	err := m.call(ctx, http.MethodGet, st.Address+PublicPrefix+"/join/"+url.PathEscape(m.d.ServerID())+"?nonce="+url.QueryEscape(st.Nonce), nil, &w)
	switch {
	case errors.Is(err, errPending):
		return nil
	case err != nil:
		var ce *callError
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			st.LastError = "the request was rejected or has expired"
			return m.setJSON(ctx, keyJoin, st)
		}
		st.LastError = err.Error()
		_ = m.setJSON(ctx, keyJoin, st)
		return err
	}
	return m.completeJoin(ctx, st, w)
}

// checkMerge refuses a merge whose same-named buckets differ in Object Lock,
// which cannot be reconciled after creation.
func (m *Manager) checkMerge(ctx context.Context, st joinState, w welcome) error {
	local, err := m.d.DB.ListBuckets(ctx, "")
	if err != nil {
		return err
	}
	if len(local) == 0 {
		return nil
	}
	ep := s3client.Endpoint{Endpoint: st.Address, AccessKey: w.Key, SecretKey: w.Secret, PathStyle: true}
	var remote []struct {
		Name       string `json:"name"`
		ObjectLock bool   `json:"object_lock"`
	}
	if err := s3client.New(ep).AdminJSON(ctx, http.MethodGet, "/buckets", nil, &remote); err != nil {
		return fmt.Errorf("list cellar buckets: %w", err)
	}
	lock := map[string]bool{}
	for _, r := range remote {
		lock[r.Name] = r.ObjectLock
	}
	var clashes []string
	for _, b := range local {
		if rl, ok := lock[b.Name]; ok && rl != b.ObjectLock {
			clashes = append(clashes, b.Name)
		}
	}
	if len(clashes) > 0 {
		return fmt.Errorf("cannot merge: Object Lock differs on %s between this server and the cellar; delete or export the local copy first", strings.Join(clashes, ", "))
	}
	return nil
}

func (m *Manager) completeJoin(ctx context.Context, st joinState, w welcome) error {
	if w.Cellar == "" || w.Key == "" || w.Secret == "" {
		return errors.New("incomplete welcome from cellar")
	}
	if err := m.checkMerge(ctx, st, w); err != nil {
		st.LastError = err.Error()
		_ = m.setJSON(ctx, keyJoin, st)
		return err
	}
	if err := m.ensureServiceAccount(ctx, w.Key, w.Secret); err != nil {
		return err
	}
	members := w.Members
	self := Member{ID: m.d.ServerID(), Name: m.d.Name(), Endpoint: st.Endpoint, Version: m.d.Version, JoinedAt: m.d.Now()}
	found := false
	for i := range members {
		if members[i].ID == self.ID {
			members[i] = self
			found = true
		}
	}
	if !found {
		members = append(members, self)
	}
	for k, v := range map[string]string{KeyID: w.Cellar, KeyName: w.Name, KeyWriteAck: w.WriteAck} {
		if err := m.set(ctx, k, v); err != nil {
			return err
		}
	}
	if err := m.saveMembers(ctx, members); err != nil {
		return err
	}
	if err := m.set(ctx, keyJoin, ""); err != nil {
		return err
	}
	// Tell the cellar the approval was consumed (best effort).
	if key, err := m.serviceKey(ctx); err == nil {
		key.Endpoint = st.Address
		_ = s3client.New(key).AdminJSON(ctx, http.MethodDelete, "/cellar/approvals/"+url.PathEscape(m.d.ServerID()), nil, nil)
	}
	m.d.Log.Info("cellar: joined", "cellar", w.Name, "members", len(members))
	return m.Reconcile(ctx)
}

// ---- join: member side ----

// pendingRequests lists outstanding join requests.
func (m *Manager) pendingRequests(ctx context.Context) ([]JoinRequest, error) {
	all, err := m.d.DB.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	var out []JoinRequest
	for k, v := range all {
		if !strings.HasPrefix(k, PendingPfx) {
			continue
		}
		var r JoinRequest
		if json.Unmarshal([]byte(v), &r) == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.Before(out[j].RequestedAt) })
	return out, nil
}

// Approve lets a pending server in; the joiner collects its credentials on
// its next poll.
func (m *Manager) Approve(ctx context.Context, id, by string) error {
	if !m.Active(ctx) {
		return ErrNotInCellar
	}
	raw := m.setting(ctx, PendingPfx+id)
	if raw == "" {
		return fmt.Errorf("no pending request %q", id)
	}
	var r JoinRequest
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return err
	}
	members, err := m.Members(ctx)
	if err != nil {
		return err
	}
	mem := Member{ID: r.ID, Name: r.Name, Endpoint: r.Endpoint, Version: r.Version, JoinedAt: m.d.Now()}
	replaced := false
	for i := range members {
		if members[i].ID == mem.ID {
			members[i] = mem
			replaced = true
		}
	}
	if !replaced {
		members = append(members, mem)
	}
	if err := m.saveMembers(ctx, members); err != nil {
		return err
	}
	if err := m.setJSON(ctx, ApprovedPfx+id, approval{ID: id, NonceHash: r.NonceHash, ApprovedAt: m.d.Now(), By: by}); err != nil {
		return err
	}
	if err := m.set(ctx, PendingPfx+id, ""); err != nil {
		return err
	}
	m.Trigger()
	return nil
}

// Reject drops a pending request.
func (m *Manager) Reject(ctx context.Context, id string) error {
	if m.setting(ctx, PendingPfx+id) == "" {
		return fmt.Errorf("no pending request %q", id)
	}
	return m.set(ctx, PendingPfx+id, "")
}

// ForgetApproval removes a consumed approval record.
func (m *Manager) ForgetApproval(ctx context.Context, id string) error {
	return m.set(ctx, ApprovedPfx+id, "")
}

// PublicHandler serves the unauthenticated join endpoints.
func (m *Manager) PublicHandler() http.Handler {
	limiter := newLimiter(30, time.Minute)
	mux := http.NewServeMux()
	mux.HandleFunc(PublicPrefix+"/join", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		ip := clientIP(r)
		if !limiter.allow(ip, m.d.Now()) {
			writeErr(w, http.StatusTooManyRequests, "too many join requests")
			return
		}
		ctx := r.Context()
		if !m.Active(ctx) {
			writeErr(w, http.StatusNotFound, "this server is not in a cellar")
			return
		}
		var req struct {
			ID, Name, Endpoint, Version, Nonce string
			Buckets                            int
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" || req.Nonce == "" {
			writeErr(w, http.StatusBadRequest, "id and nonce are required")
			return
		}
		if req.ID == m.d.ServerID() {
			writeErr(w, http.StatusBadRequest, "a server cannot join itself")
			return
		}
		members, _ := m.Members(ctx)
		for _, mem := range members {
			if mem.ID == req.ID {
				writeErr(w, http.StatusConflict, "already a member")
				return
			}
		}
		endpoint := strings.TrimSpace(req.Endpoint)
		if endpoint == "" || strings.Contains(endpoint, "0.0.0.0") {
			// Derive from where the request came from and the advertised port.
			port := "9000"
			if u, err := url.Parse(endpoint); err == nil && u.Port() != "" {
				port = u.Port()
			}
			endpoint = "http://" + net.JoinHostPort(ip, port)
		}
		jr := JoinRequest{ID: req.ID, Name: strings.TrimSpace(req.Name), Endpoint: endpoint, Version: req.Version, NonceHash: hashNonce(req.Nonce), RequestedAt: m.d.Now(), Buckets: req.Buckets}
		if err := m.setJSON(ctx, PendingPfx+req.ID, jr); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		m.d.Log.Info("cellar: join request", "from", jr.Name, "id", jr.ID, "endpoint", jr.Endpoint)
		writeJSON(w, http.StatusOK, map[string]string{"cellar": m.setting(ctx, KeyID), "name": m.setting(ctx, KeyName)})
	})
	mux.HandleFunc(PublicPrefix+"/join/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		ctx := r.Context()
		id := strings.TrimPrefix(r.URL.Path, PublicPrefix+"/join/")
		nonce := r.URL.Query().Get("nonce")
		if id == "" || nonce == "" {
			writeErr(w, http.StatusBadRequest, "id and nonce are required")
			return
		}
		if raw := m.setting(ctx, PendingPfx+id); raw != "" {
			var jr JoinRequest
			if json.Unmarshal([]byte(raw), &jr) == nil && subtle.ConstantTimeCompare([]byte(jr.NonceHash), []byte(hashNonce(nonce))) == 1 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
		}
		raw := m.setting(ctx, ApprovedPfx+id)
		if raw == "" {
			writeErr(w, http.StatusNotFound, "no such request")
			return
		}
		var ap approval
		if json.Unmarshal([]byte(raw), &ap) != nil || subtle.ConstantTimeCompare([]byte(ap.NonceHash), []byte(hashNonce(nonce))) != 1 {
			writeErr(w, http.StatusNotFound, "no such request")
			return
		}
		key, err := m.serviceKey(ctx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		members, _ := m.Members(ctx)
		writeJSON(w, http.StatusOK, welcome{Cellar: m.setting(ctx, KeyID), Name: m.setting(ctx, KeyName), Members: members, Key: key.AccessKey, Secret: key.SecretKey, WriteAck: m.setting(ctx, KeyWriteAck)})
	})
	return mux
}

func hashNonce(n string) string {
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": http.StatusText(status), "message": msg}})
}

// ---- readiness ----

// checkBacklog asks every other member how many changes it still has to
// push to us; the server is ready once that has been zero twice in a row.
func (m *Manager) checkBacklog(ctx context.Context) {
	if !m.Active(ctx) {
		m.mu.Lock()
		m.ready, m.backlog, m.checkErr = true, 0, ""
		m.mu.Unlock()
		return
	}
	members, err := m.Members(ctx)
	if err != nil {
		return
	}
	key, err := m.serviceKey(ctx)
	if err != nil {
		return
	}
	self := TargetPrefix + m.d.ServerID()
	var total int64
	var firstErr string
	for _, mem := range members {
		if mem.ID == m.d.ServerID() {
			continue
		}
		ep := key
		ep.Endpoint = mem.Endpoint
		var rows []replication.TargetStatus
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s3client.New(ep).AdminJSON(cctx, http.MethodGet, "/replication", nil, &rows)
		cancel()
		if err != nil {
			if firstErr == "" {
				firstErr = mem.Name + ": " + err.Error()
			}
			continue
		}
		for _, r := range rows {
			if r.Target.Name == self {
				total += r.Lag
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backlog, m.lastCheck, m.checkErr = total, m.d.Now(), firstErr
	if total == 0 && firstErr == "" {
		m.zeroStreak++
	} else {
		m.zeroStreak = 0
	}
	m.ready = m.zeroStreak >= 2 || (len(members) <= 1)
}

// Ready reports whether this member has caught up with its peers.
func (m *Manager) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

// Status assembles the admin view.
func (m *Manager) Status(ctx context.Context) (*Status, error) {
	st := &Status{SelfID: m.d.ServerID(), Members: []MemberStatus{}, Pending: []JoinRequest{}}
	if raw := m.setting(ctx, keyJoin); raw != "" {
		var js joinState
		if json.Unmarshal([]byte(raw), &js) == nil {
			js.Nonce = ""
			st.Joining = &js
		}
	}
	m.mu.Lock()
	st.Ready, st.Backlog, st.LastCheck, st.CheckError = m.ready, m.backlog, m.lastCheck, m.checkErr
	st.Notices = append([]Notice{}, m.notices...)
	backfilling := map[string]bool{}
	for k := range m.backfilling {
		backfilling[k] = true
	}
	m.mu.Unlock()
	if !m.Active(ctx) {
		return st, nil
	}
	st.Active = true
	st.ID, st.Name, st.WriteAck = m.setting(ctx, KeyID), m.setting(ctx, KeyName), m.setting(ctx, KeyWriteAck)
	members, err := m.Members(ctx)
	if err != nil {
		return nil, err
	}
	lag := map[string]*replication.TargetStatus{}
	for _, r := range m.d.Worker.Status(ctx) {
		if !r.Target.Cellar {
			continue
		}
		cur := lag[r.Target.Name]
		if cur == nil {
			cp := r
			lag[r.Target.Name] = &cp
			continue
		}
		cur.Lag += r.Lag
		if r.LastError != "" {
			cur.LastError = r.LastError
		}
	}
	for _, mem := range members {
		ms := MemberStatus{Member: mem, Self: mem.ID == m.d.ServerID(), Enabled: true}
		if !ms.Self {
			ms.Joining = m.stillJoining(ctx, mem.ID)
			if r := lag[TargetPrefix+mem.ID]; r != nil && !ms.Joining {
				ms.Lag, ms.LastError, ms.Enabled = r.Lag, r.LastError, r.Target.Enabled
			}
			ms.Backfilling = backfilling[mem.ID]
			ms.Backfilled = m.setting(ctx, backfilledPfx+mem.ID) != ""
		}
		st.Members = append(st.Members, ms)
	}
	if p, err := m.pendingRequests(ctx); err == nil && p != nil {
		st.Pending = p
	}
	return st, nil
}

// ---- small rate limiter for the public endpoint ----

type limiter struct {
	mu    sync.Mutex
	hits  map[string][]time.Time
	burst int
	win   time.Duration
}

func newLimiter(burst int, win time.Duration) *limiter {
	return &limiter{hits: map[string][]time.Time{}, burst: burst, win: win}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keep []time.Time
	for _, t := range l.hits[ip] {
		if now.Sub(t) < l.win {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.burst {
		l.hits[ip] = keep
		return false
	}
	l.hits[ip] = append(keep, now)
	return true
}

// SelfEndpoint derives how peers can reach this server: the configured
// advertise address, else the first non-loopback IPv4 with the listen port.
func SelfEndpoint(advertise string, listen []string, tls bool) string {
	if advertise != "" {
		return advertise
	}
	port := "9000"
	if len(listen) > 0 {
		if _, p, err := net.SplitHostPort(listen[0]); err == nil && p != "" {
			port = p
		}
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	ip := "127.0.0.1"
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				ip = ipn.IP.String()
				break
			}
		}
	}
	return scheme + "://" + net.JoinHostPort(ip, port)
}
