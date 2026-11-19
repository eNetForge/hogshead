package admin

import (
	"errors"
	"net/http"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/transfer"
)

func (h *Handler) transferRoutes() []Route {
	return []Route{
		{"GET", "/transfers", "Import/export jobs with progress, newest first", true, h.listTransfers},
		{"POST", "/transfers", "Start an import (remote to local) or export (local to remote) job", true, h.startTransfer},
		{"GET", "/transfers/{id}", "One job with progress and first errors", true, h.getTransfer},
		{"DELETE", "/transfers/{id}", "Cancel a running job, or remove a finished job's record", true, h.deleteTransfer},
	}
}

func (h *Handler) transfers() (*transfer.Manager, error) {
	if h.d.Transfers == nil {
		return nil, Errorf(http.StatusServiceUnavailable, "ServiceUnavailable", "transfers are not enabled on this server")
	}
	return h.d.Transfers, nil
}

func (h *Handler) listTransfers(c *Call) error {
	m, err := h.transfers()
	if err != nil {
		return err
	}
	jobs, err := m.List(c.Ctx())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, jobs)
}

func (h *Handler) startTransfer(c *Call) error {
	m, err := h.transfers()
	if err != nil {
		return err
	}
	var req TransferRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	pathStyle := true
	if req.PathStyle != nil {
		pathStyle = *req.PathStyle
	}
	spec := transfer.Spec{
		Direction: transfer.Direction(req.Direction), Bucket: req.Bucket, Prefix: req.Prefix,
		Remote:       s3client.Endpoint{Endpoint: req.Endpoint, Bucket: req.RemoteBucket, AccessKey: req.AccessKey, SecretKey: req.SecretKey, Region: req.Region, PathStyle: pathStyle},
		RemotePrefix: req.RemotePrefix, Overwrite: req.Overwrite, DryRun: req.DryRun, Concurrency: req.Concurrency,
	}
	if c.User != nil {
		spec.OwnerID = c.User.ID
	}
	job, err := m.Start(c.Ctx(), spec, c.Actor())
	if err != nil {
		var me *meta.Error
		if errors.As(err, &me) {
			return err
		}
		return Errorf(http.StatusBadRequest, "InvalidArgument", "%v", err)
	}
	h.audit(c, "transfer.start", job.ID, string(job.Spec.Direction)+" "+job.Spec.Bucket+" <-> "+job.Spec.Remote.Endpoint+"/"+job.Spec.Remote.Bucket)
	return c.JSON(http.StatusAccepted, job)
}

func (h *Handler) getTransfer(c *Call) error {
	m, err := h.transfers()
	if err != nil {
		return err
	}
	job, err := m.Get(c.Ctx(), c.Params["id"])
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, job)
}

func (h *Handler) deleteTransfer(c *Call) error {
	m, err := h.transfers()
	if err != nil {
		return err
	}
	id := c.Params["id"]
	if err := m.Cancel(id); err == nil {
		h.audit(c, "transfer.cancel", id, "")
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	}
	if err := m.Delete(c.Ctx(), id); err != nil {
		var me *meta.Error
		if errors.As(err, &me) {
			return err
		}
		return Errorf(http.StatusConflict, "Conflict", "%v", err)
	}
	h.audit(c, "transfer.delete", id, "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}
