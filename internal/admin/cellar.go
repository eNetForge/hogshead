package admin

import (
	"errors"
	"net/http"

	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/s3client"
)

// CellarInitRequest creates a cellar.
type CellarInitRequest struct {
	Name     string `json:"name"`
	WriteAck string `json:"write_ack,omitempty"` // local | one (default) | all
}

// CellarJoinRequest asks a member to let this server in.
type CellarJoinRequest struct {
	Address   string `json:"address"`             // host[:port] or URL of any member
	Advertise string `json:"advertise,omitempty"` // how members reach this server; default derived
	// Merge lets a server that already has buckets join; they are added to
	// the cellar. Without it a non-empty server is refused.
	Merge bool `json:"merge,omitempty"`
}

// CellarUpdateRequest changes the name or acknowledgement level.
type CellarUpdateRequest struct {
	Name     string `json:"name,omitempty"`
	WriteAck string `json:"write_ack,omitempty"`
}

func (h *Handler) cellarRoutes() []Route {
	return []Route{
		{"GET", "/cellar", "Cellar membership, member lag, pending join requests and catch-up state", true, h.cellarStatus},
		{"POST", "/cellar/init", "Create a cellar with this server as its first member", true, h.cellarInit},
		{"PUT", "/cellar", "Change the cellar name or write acknowledgement level", true, h.cellarUpdate},
		{"POST", "/cellar/join", "Ask a member to admit this server; an admin there must approve", true, h.cellarJoin},
		{"DELETE", "/cellar/join", "Withdraw this server's outstanding join request", true, h.cellarCancelJoin},
		{"POST", "/cellar/pending/{id}/approve", "Approve a pending join request", true, h.cellarApprove},
		{"DELETE", "/cellar/pending/{id}", "Reject a pending join request", true, h.cellarReject},
		{"DELETE", "/cellar/members/{id}", "Remove a member and rotate the cellar key", true, h.cellarRemove},
		{"POST", "/cellar/leave", "Leave the cellar", true, h.cellarLeave},
		{"DELETE", "/cellar/approvals/{id}", "Forget a consumed join approval (called by the joiner)", true, h.cellarForgetApproval},
	}
}

// cellarActive reports whether this server is in a cellar.
func (h *Handler) cellarActive(c *Call) bool {
	return h.d.Cellar != nil && h.d.Cellar.Active(c.Ctx())
}

// protectUser refuses changes to the cellar service account while the server
// is in a cellar; the cellar rotates and removes it itself.
func (h *Handler) protectUser(c *Call, name string) error {
	if name == cellar.ServiceUser && h.cellarActive(c) {
		return Errorf(http.StatusConflict, "CellarManaged", "the %q account is the cellar's service account and is managed by the cellar; remove members or leave the cellar instead", name)
	}
	return nil
}

// reservedUser refuses creating the cellar service account by hand: inside a
// cellar it already exists, outside one a user of that name would be adopted
// (and made an administrator) by a later init or join.
func (h *Handler) reservedUser(c *Call, name string) error {
	if name != cellar.ServiceUser || h.d.Cellar == nil {
		return nil
	}
	if h.cellarActive(c) && c.R.Header.Get(s3client.OriginHeader) != "" {
		return nil // configuration sync from another member
	}
	return Errorf(http.StatusConflict, "ReservedUser", "%q is reserved for the cellar's service account and is created by 'cellar init' or a join", name)
}

// protectKey refuses changes to the cellar service account's keys.
func (h *Handler) protectKey(c *Call, ak string) error {
	if !h.cellarActive(c) {
		return nil
	}
	cred, err := h.d.DB.LookupAccessKey(c.Ctx(), ak)
	if err == nil && cred.User.Name == cellar.ServiceUser {
		return Errorf(http.StatusConflict, "CellarManaged", "access key %s belongs to the cellar's service account; the cellar rotates it when a member is removed", ak)
	}
	return nil
}

// protectEndpoint refuses manual replication targets that point at a cellar
// member, which the cellar already replicates to.
func (h *Handler) protectEndpoint(c *Call, endpoint string) error {
	if endpoint == "" || h.d.Cellar == nil {
		return nil
	}
	if mem := h.d.Cellar.MemberFor(c.Ctx(), endpoint); mem != nil {
		return Errorf(http.StatusConflict, "CellarMember", "%s is the cellar member %q; replication between members is managed by the cellar and a manual target would push everything twice", endpoint, mem.Name)
	}
	return nil
}

func (h *Handler) cellarMgr() (*cellar.Manager, error) {
	if h.d.Cellar == nil {
		return nil, Errorf(http.StatusServiceUnavailable, "ServiceUnavailable", "cellar is not enabled on this server")
	}
	return h.d.Cellar, nil
}

func cellarErr(err error) error {
	if errors.Is(err, cellar.ErrNotInCellar) {
		return Errorf(http.StatusConflict, "NotInCellar", "%v", err)
	}
	return Errorf(http.StatusBadRequest, "InvalidArgument", "%v", err)
}

func (h *Handler) cellarStatus(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	st, err := m.Status(c.Ctx())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, st)
}

func (h *Handler) cellarInit(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	var req CellarInitRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	st, err := m.Init(c.Ctx(), req.Name, req.WriteAck)
	if err != nil {
		return cellarErr(err)
	}
	h.audit(c, "cellar.init", st.Name, st.ID)
	return c.JSON(http.StatusCreated, st)
}

func (h *Handler) cellarUpdate(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	var req CellarUpdateRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	st, err := m.Update(c.Ctx(), req.Name, req.WriteAck)
	if err != nil {
		return cellarErr(err)
	}
	h.audit(c, "cellar.update", st.Name, req.WriteAck)
	return c.JSON(http.StatusOK, st)
}

func (h *Handler) cellarJoin(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	var req CellarJoinRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	st, err := m.Join(c.Ctx(), req.Address, req.Advertise, req.Merge)
	if err != nil {
		var ne *cellar.ErrNotEmpty
		if errors.As(err, &ne) {
			return Errorf(http.StatusConflict, "NotEmpty", "%v", err)
		}
		return cellarErr(err)
	}
	h.audit(c, "cellar.join", req.Address, "")
	return c.JSON(http.StatusAccepted, st)
}

func (h *Handler) cellarCancelJoin(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	if err := m.CancelJoin(c.Ctx()); err != nil {
		return err
	}
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) cellarApprove(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	if err := m.Approve(c.Ctx(), c.Params["id"], c.Actor()); err != nil {
		return cellarErr(err)
	}
	h.audit(c, "cellar.approve", c.Params["id"], "")
	st, err := m.Status(c.Ctx())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, st)
}

func (h *Handler) cellarReject(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	if err := m.Reject(c.Ctx(), c.Params["id"]); err != nil {
		return Errorf(http.StatusNotFound, "NotFound", "%v", err)
	}
	h.audit(c, "cellar.reject", c.Params["id"], "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) cellarRemove(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	if err := m.Remove(c.Ctx(), c.Params["id"]); err != nil {
		return cellarErr(err)
	}
	h.audit(c, "cellar.remove", c.Params["id"], "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) cellarLeave(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	if err := m.Leave(c.Ctx()); err != nil {
		return cellarErr(err)
	}
	h.audit(c, "cellar.leave", "", "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) cellarForgetApproval(c *Call) error {
	m, err := h.cellarMgr()
	if err != nil {
		return err
	}
	if err := m.ForgetApproval(c.Ctx(), c.Params["id"]); err != nil {
		return err
	}
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}
