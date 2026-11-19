package api

import (
	"net/http"
	"strings"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/notify"
)

func notificationKey(bucket string) string { return "bucket.notification:" + bucket }

func (h *Handler) notificationConfig(c *reqCtx) *s3xml.NotificationConfiguration {
	raw, err := h.d.DB.GetSetting(c.ctx(), notificationKey(c.bucket.Name))
	if err != nil || raw == "" {
		return nil
	}
	var cfg s3xml.NotificationConfiguration
	if s3xml.Unmarshal(strings.NewReader(raw), &cfg, s3xml.DefaultLimit) != nil {
		return nil
	}
	return &cfg
}

func (h *Handler) getBucketNotification(c *reqCtx) error {
	cfg := h.notificationConfig(c)
	if cfg == nil {
		cfg = &s3xml.NotificationConfiguration{}
	}
	return c.xml(http.StatusOK, cfg)
}

func (h *Handler) putBucketNotification(c *reqCtx) error {
	var cfg s3xml.NotificationConfiguration
	if c.r.ContentLength != 0 {
		if err := parseXMLBody(c, &cfg); err != nil {
			return err
		}
	}
	for i := range cfg.QueueConfiguration {
		q := &cfg.QueueConfiguration[i]
		if !strings.HasPrefix(q.Queue, notify.ARNPrefix) {
			return s3err.New(s3err.InvalidArgument).WithMessage("Queue must be a webhook ARN of the form " + notify.ARNPrefix + "<name>")
		}
		name := strings.TrimPrefix(q.Queue, notify.ARNPrefix)
		if u, _ := h.d.DB.GetSetting(c.ctx(), notify.SettingPrefix+name); u == "" {
			return s3err.New(s3err.InvalidArgument).WithMessage("Unknown webhook " + name + "; create it with: hogshead setting set " + notify.SettingPrefix + name + " https://...")
		}
		if len(q.Event) == 0 {
			return s3err.New(s3err.MalformedXML).WithMessage("QueueConfiguration needs at least one Event")
		}
		for _, e := range q.Event {
			if !notify.ValidEvent(e) {
				return s3err.New(s3err.InvalidArgument).WithMessage("Unsupported event " + e)
			}
		}
		if q.ID == "" {
			q.ID = name
		}
	}
	raw := ""
	if len(cfg.QueueConfiguration) > 0 {
		b, err := s3xml.Marshal(&cfg)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	if _, err := h.propose(c, &meta.SetSetting{Key: notificationKey(c.bucket.Name), Value: raw}); err != nil {
		return err
	}
	return c.empty(http.StatusOK)
}

// emit sends an event for the current request's bucket when configured.
func (h *Handler) emit(c *reqCtx, name, key string, size int64, etag, vid string) {
	if h.d.Notify == nil || c.bucket == nil {
		return
	}
	cfg := h.notificationConfig(c)
	if cfg == nil {
		return
	}
	if vid == meta.NullVersionID {
		vid = ""
	}
	h.d.Notify.Emit(cfg, notify.Event{
		Name: name, Bucket: c.bucket.Name, Key: key, Size: size, ETag: etag, VersionID: vid,
		Time: h.now(), Principal: c.caller.Name(), SourceIP: clientIP(c.r), Region: h.d.Region,
	})
}
