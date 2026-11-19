// Package notify delivers S3 event notifications to webhooks. Destinations
// are server settings named webhook.<name> holding a URL; bucket
// notification configurations reference them as arn:hogshead:webhook:::<name>.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3xml"
)

// ARNPrefix identifies webhook destinations.
const ARNPrefix = "arn:hogshead:webhook:::"

// SettingPrefix is the setting prefix for webhook URLs.
const SettingPrefix = "webhook."

// Event is one S3 event to deliver.
type Event struct {
	Name      string // e.g. s3:ObjectCreated:Put
	Bucket    string
	Key       string
	Size      int64
	ETag      string
	VersionID string
	Sequencer string
	Time      time.Time
	Principal string
	SourceIP  string
	Region    string
}

// Record is the AWS-shaped JSON record.
type Record struct {
	EventVersion      string            `json:"eventVersion"`
	EventSource       string            `json:"eventSource"`
	AwsRegion         string            `json:"awsRegion"`
	EventTime         string            `json:"eventTime"`
	EventName         string            `json:"eventName"`
	UserIdentity      map[string]string `json:"userIdentity"`
	RequestParameters map[string]string `json:"requestParameters"`
	ResponseElements  map[string]string `json:"responseElements"`
	S3                struct {
		S3SchemaVersion string `json:"s3SchemaVersion"`
		ConfigurationID string `json:"configurationId"`
		Bucket          struct {
			Name          string            `json:"name"`
			OwnerIdentity map[string]string `json:"ownerIdentity"`
			ARN           string            `json:"arn"`
		} `json:"bucket"`
		Object struct {
			Key       string `json:"key"`
			Size      int64  `json:"size,omitempty"`
			ETag      string `json:"eTag,omitempty"`
			VersionID string `json:"versionId,omitempty"`
			Sequencer string `json:"sequencer"`
		} `json:"object"`
	} `json:"s3"`
}

// Payload is the webhook body.
type Payload struct {
	Records []Record `json:"Records"`
}

// Lookup resolves a webhook name to its URL (and optional secret); ok=false
// when unknown.
type Lookup func(ctx context.Context, name string) (url, secret string, ok bool)

// Dispatcher queues and delivers events.
type Dispatcher struct {
	lookup Lookup
	log    *slog.Logger
	client *http.Client
	queue  chan delivery
	wg     sync.WaitGroup
	stop   chan struct{}
	once   sync.Once
}

type delivery struct {
	name     string
	payload  []byte
	attempt  int
	configID string
}

// New creates a dispatcher with the given worker count.
func New(lookup Lookup, log *slog.Logger, workers int) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	if workers <= 0 {
		workers = 2
	}
	d := &Dispatcher{lookup: lookup, log: log, client: &http.Client{Timeout: 15 * time.Second}, queue: make(chan delivery, 4096), stop: make(chan struct{})}
	for i := 0; i < workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	return d
}

// Close stops delivery after draining what is queued.
func (d *Dispatcher) Close() {
	d.once.Do(func() { close(d.stop) })
	d.wg.Wait()
}

// Matches reports whether a configuration selects an event name and key.
func Matches(q *s3xml.QueueConfiguration, eventName, key string) bool {
	ok := false
	for _, e := range q.Event {
		if e == eventName || strings.HasSuffix(e, ":*") && strings.HasPrefix(eventName, strings.TrimSuffix(e, "*")) {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	if q.Filter != nil {
		for _, r := range q.Filter.S3Key.FilterRule {
			switch strings.ToLower(r.Name) {
			case "prefix":
				if !strings.HasPrefix(key, r.Value) {
					return false
				}
			case "suffix":
				if !strings.HasSuffix(key, r.Value) {
					return false
				}
			}
		}
	}
	return true
}

// Emit evaluates a bucket's configuration against an event and queues
// deliveries. It never blocks the caller for long.
func (d *Dispatcher) Emit(cfg *s3xml.NotificationConfiguration, ev Event) {
	if d == nil || cfg == nil {
		return
	}
	for i := range cfg.QueueConfiguration {
		q := &cfg.QueueConfiguration[i]
		if !strings.HasPrefix(q.Queue, ARNPrefix) || !Matches(q, ev.Name, ev.Key) {
			continue
		}
		rec := buildRecord(ev, q.ID)
		body, err := json.Marshal(Payload{Records: []Record{rec}})
		if err != nil {
			continue
		}
		select {
		case d.queue <- delivery{name: strings.TrimPrefix(q.Queue, ARNPrefix), payload: body, configID: q.ID}:
		default:
			d.log.Warn("notification queue full, dropping event", "bucket", ev.Bucket, "key", ev.Key, "event", ev.Name)
		}
	}
}

func buildRecord(ev Event, configID string) Record {
	region := ev.Region
	if region == "" {
		region = "us-east-1"
	}
	r := Record{
		EventVersion: "2.1", EventSource: "hogshead:s3", AwsRegion: region,
		EventTime: ev.Time.UTC().Format("2006-01-02T15:04:05.000Z"), EventName: strings.TrimPrefix(ev.Name, "s3:"),
		UserIdentity:      map[string]string{"principalId": ev.Principal},
		RequestParameters: map[string]string{"sourceIPAddress": ev.SourceIP},
		ResponseElements:  map[string]string{},
	}
	r.S3.S3SchemaVersion = "1.0"
	r.S3.ConfigurationID = configID
	r.S3.Bucket.Name = ev.Bucket
	r.S3.Bucket.OwnerIdentity = map[string]string{"principalId": ev.Principal}
	r.S3.Bucket.ARN = "arn:aws:s3:::" + ev.Bucket
	r.S3.Object.Key = url.QueryEscape(ev.Key)
	r.S3.Object.Size = ev.Size
	r.S3.Object.ETag = strings.Trim(ev.ETag, `"`)
	r.S3.Object.VersionID = ev.VersionID
	r.S3.Object.Sequencer = ev.Sequencer
	if r.S3.Object.Sequencer == "" {
		r.S3.Object.Sequencer = strings.ToUpper(hex.EncodeToString([]byte{byte(ev.Time.UnixNano() >> 56), byte(ev.Time.UnixNano() >> 48), byte(ev.Time.UnixNano() >> 40), byte(ev.Time.UnixNano() >> 32), byte(ev.Time.UnixNano() >> 24), byte(ev.Time.UnixNano() >> 16), byte(ev.Time.UnixNano() >> 8), byte(ev.Time.UnixNano())}))
	}
	return r
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stop:
			// Drain quickly without retries.
			for {
				select {
				case dv := <-d.queue:
					_ = d.send(dv)
				default:
					return
				}
			}
		case dv := <-d.queue:
			if err := d.send(dv); err != nil {
				if dv.attempt < 5 {
					dv.attempt++
					delay := time.Duration(1<<dv.attempt) * time.Second
					d.log.Warn("webhook delivery failed, will retry", "webhook", dv.name, "attempt", dv.attempt, "err", err)
					time.AfterFunc(delay, func() {
						select {
						case d.queue <- dv:
						default:
						}
					})
				} else {
					d.log.Error("webhook delivery gave up", "webhook", dv.name, "err", err)
				}
			}
		}
	}
}

func (d *Dispatcher) send(dv delivery) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	target, secret, ok := d.lookup(ctx, dv.name)
	if !ok {
		return &Error{Msg: "webhook " + dv.name + " is not configured"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(dv.payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hogshead-webhook")
	if secret != "" {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(dv.payload)
		req.Header.Set("X-Hogshead-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &Error{Msg: "webhook returned " + resp.Status}
	}
	return nil
}

// Error is a delivery error.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// ValidEvent reports whether an event name is one we emit.
func ValidEvent(e string) bool {
	switch e {
	case "s3:ObjectCreated:*", "s3:ObjectCreated:Put", "s3:ObjectCreated:Post", "s3:ObjectCreated:Copy", "s3:ObjectCreated:CompleteMultipartUpload",
		"s3:ObjectRemoved:*", "s3:ObjectRemoved:Delete", "s3:ObjectRemoved:DeleteMarkerCreated", "s3:ObjectTagging:*", "s3:ObjectTagging:Put", "s3:ObjectTagging:Delete",
		"s3:*":
		return true
	}
	return false
}
