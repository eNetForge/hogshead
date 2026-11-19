// Package usage turns finished S3 requests into two things: an optional
// access log (one JSON line per request, reopened on reload so logrotate
// works) and hourly counters per bucket, user, access key and operation
// class, kept in the metadata store for the Usage page, the CLI and the API.
package usage

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

// Record is one finished S3 request.
type Record struct {
	Time      time.Time `json:"time"`
	ID        string    `json:"id"`
	Op        string    `json:"op"`
	Method    string    `json:"method"`
	Bucket    string    `json:"bucket,omitempty"`
	Key       string    `json:"key,omitempty"`
	Status    int       `json:"status"`
	BytesIn   int64     `json:"bytes_in"`
	BytesOut  int64     `json:"bytes_out"`
	Duration  int64     `json:"duration_ms"`
	User      string    `json:"user,omitempty"`
	AccessKey string    `json:"access_key,omitempty"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent,omitempty"`
	Referer   string    `json:"referer,omitempty"`
	// ErrorCode and ErrorMessage are the S3 error returned, for failed requests.
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// Replicated marks a request made by another hogshead's replication
	// (it carried an origin); such traffic is counted under its own class.
	Replicated bool `json:"replicated,omitempty"`
}

// Classes group operations for the counters.
const (
	ClassRead   = "read"
	ClassWrite  = "write"
	ClassDelete = "delete"
	ClassList   = "list"
	ClassBucket = "bucket"
	ClassOther  = "other"
	// ClassReplication is traffic pushed by other hogshead servers.
	ClassReplication = "replication"
)

// Classify maps an S3 operation name to a class.
func Classify(op string) string {
	switch {
	case op == "", op == "Unknown", op == "Options":
		return ClassOther
	case strings.HasPrefix(op, "List"):
		return ClassList
	case op == "CreateBucket", op == "DeleteBucket", op == "HeadBucket", strings.Contains(op, "Bucket"):
		return ClassBucket
	case strings.HasPrefix(op, "Get"), strings.HasPrefix(op, "Head"):
		return ClassRead
	case strings.HasPrefix(op, "Delete"), strings.HasPrefix(op, "Abort"):
		return ClassDelete
	case strings.HasPrefix(op, "Put"), strings.HasPrefix(op, "Post"), strings.HasPrefix(op, "Copy"),
		strings.HasPrefix(op, "Create"), strings.HasPrefix(op, "Upload"), strings.HasPrefix(op, "Complete"):
		return ClassWrite
	}
	return ClassOther
}

// Service receives records, writes the access log and aggregates counters.
type Service struct {
	propose func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	rows    map[string]*meta.UsageRow
	errs    []meta.ErrorRow // failed requests waiting for the next flush
	logPath string
	file    *os.File
	queue   chan []byte
	dropped int64
}

// New builds a service that aggregates into the metadata store via propose.
func New(propose func(ctx context.Context, cmd meta.Command) (meta.Result, error), log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{propose: propose, log: log, now: time.Now, rows: map[string]*meta.UsageRow{}, queue: make(chan []byte, 4096)}
	return s
}

// SetAccessLog points the access log at a file ("" disables it). Calling it
// with the current path reopens the file, which is what a reload after
// logrotate needs.
func (s *Service) SetAccessLog(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	s.logPath = path
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		s.log.Error("access log: create directory", "path", path, "err", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640) //nolint:gosec // operator-configured path
	if err != nil {
		s.log.Error("access log: open", "path", path, "err", err)
		return
	}
	s.file = f
}

// AccessLogPath returns the configured path.
func (s *Service) AccessLogPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logPath
}

// Handle records one request. It never blocks the request path: the log
// line is queued and dropped (and counted) when the queue is full.
func (s *Service) Handle(r Record) {
	s.mu.Lock()
	hour := r.Time.UTC().Truncate(time.Hour).Unix()
	class := Classify(r.Op)
	if r.Replicated {
		class = ClassReplication
	}
	k := strings.Join([]string{strconv(hour), r.Bucket, r.User, r.AccessKey, class}, "\x00")
	row := s.rows[k]
	if row == nil {
		row = &meta.UsageRow{Hour: hour, Bucket: r.Bucket, User: r.User, AccessKey: r.AccessKey, Class: class}
		s.rows[k] = row
	}
	row.Requests++
	if r.Status >= 400 {
		row.Errors++
	}
	row.BytesIn += r.BytesIn
	row.BytesOut += r.BytesOut
	if r.Status >= 400 && len(s.errs) < maxPendingErrors {
		if d, err := json.Marshal(r); err == nil { //nolint:gosec // G117: key id, not a secret
			s.errs = append(s.errs, meta.ErrorRow{At: r.Time.UnixMilli(), Data: string(d)})
		}
	}
	logging := s.file != nil
	s.mu.Unlock()
	if !logging {
		return
	}
	line, err := json.Marshal(r) //nolint:gosec // G117: the access key *id* is a public identifier; secrets are never in a Record
	if err != nil {
		return
	}
	select {
	case s.queue <- append(line, '\n'):
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

func strconv(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

const (
	maxPendingErrors = 2000 // per flush interval; beyond that only counters record them
	// KeepErrors is how many failed requests each server remembers.
	KeepErrors = 10000
)

// Flush writes the aggregated counters and pending failed requests to the store.
func (s *Service) Flush(ctx context.Context) error {
	s.mu.Lock()
	errs := s.errs
	s.errs = nil
	s.mu.Unlock()
	if len(errs) > 0 {
		res, err := s.propose(ctx, &meta.AddErrors{Rows: errs, Keep: KeepErrors})
		if err == nil {
			err = res.Error()
		}
		if err != nil {
			s.log.Warn("usage: storing failed requests", "err", err)
		}
	}
	s.mu.Lock()
	if len(s.rows) == 0 {
		s.mu.Unlock()
		return nil
	}
	rows := make([]meta.UsageRow, 0, len(s.rows))
	for _, r := range s.rows {
		rows = append(rows, *r)
	}
	s.rows = map[string]*meta.UsageRow{}
	s.mu.Unlock()
	res, err := s.propose(ctx, &meta.AddUsage{Rows: rows})
	if err == nil {
		err = res.Error()
	}
	if err != nil {
		// Put the counters back so nothing is lost on a transient failure.
		s.mu.Lock()
		for i := range rows {
			r := rows[i]
			k := strings.Join([]string{strconv(r.Hour), r.Bucket, r.User, r.AccessKey, r.Class}, "\x00")
			if cur := s.rows[k]; cur != nil {
				cur.Requests += r.Requests
				cur.Errors += r.Errors
				cur.BytesIn += r.BytesIn
				cur.BytesOut += r.BytesOut
			} else {
				s.rows[k] = &r
			}
		}
		s.mu.Unlock()
	}
	return err
}

// Prune deletes counters older than the retention.
func (s *Service) Prune(ctx context.Context, retention time.Duration) error {
	res, err := s.propose(ctx, &meta.PruneUsage{Before: s.now().Add(-retention).Unix()})
	if err != nil {
		return err
	}
	return res.Error()
}

// Run flushes every interval, drains the access log queue, and prunes once
// an hour using retention(), until ctx ends. A final flush happens on exit.
func (s *Service) Run(ctx context.Context, interval time.Duration, retention func() time.Duration) {
	flush := time.NewTicker(interval)
	defer flush.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			s.drain()
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Flush(fctx)
			cancel()
			s.mu.Lock()
			if s.file != nil {
				_ = s.file.Close()
				s.file = nil
			}
			s.mu.Unlock()
			return
		case line := <-s.queue:
			s.write(line)
		case <-flush.C:
			if err := s.Flush(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("usage: flush failed", "err", err)
			}
			s.mu.Lock()
			d := s.dropped
			s.dropped = 0
			s.mu.Unlock()
			if d > 0 {
				s.log.Warn("access log: lines dropped because the writer could not keep up", "dropped", d)
			}
		case <-prune.C:
			if r := retention(); r > 0 {
				if err := s.Prune(ctx, r); err != nil && ctx.Err() == nil {
					s.log.Warn("usage: prune failed", "err", err)
				}
			}
		}
	}
}

func (s *Service) drain() {
	for {
		select {
		case line := <-s.queue:
			s.write(line)
		default:
			return
		}
	}
}

func (s *Service) write(line []byte) {
	s.mu.Lock()
	f := s.file
	s.mu.Unlock()
	if f == nil {
		return
	}
	if _, err := f.Write(line); err != nil {
		s.log.Error("access log: write", "err", err)
	}
}
