// Package metrics exposes Prometheus metrics for the node.
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the registry and instruments.
type Metrics struct {
	reg *prometheus.Registry

	Requests      *prometheus.CounterVec
	Duration      *prometheus.HistogramVec
	BytesIn       prometheus.Counter
	BytesOut      prometheus.Counter
	AuthFailures  prometheus.Counter
	Objects       prometheus.Gauge
	Bytes         prometheus.Gauge
	Buckets       prometheus.Gauge
	Uploads       prometheus.Gauge
	ChunksOnDisk  prometheus.Gauge
	WorkerCounter *prometheus.GaugeVec

	once sync.Once
}

// New creates a registry with the standard Go and process collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{reg: reg}
	m.Requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hogshead_s3_requests_total", Help: "S3 requests by operation and status class."}, []string{"op", "status"})
	m.Duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "hogshead_s3_request_seconds", Help: "S3 request latency.", Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}}, []string{"op"})
	m.BytesIn = prometheus.NewCounter(prometheus.CounterOpts{Name: "hogshead_s3_bytes_in_total", Help: "Object bytes received."})
	m.BytesOut = prometheus.NewCounter(prometheus.CounterOpts{Name: "hogshead_s3_bytes_out_total", Help: "Object bytes sent."})
	m.AuthFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "hogshead_s3_auth_failures_total", Help: "Failed authentication attempts."})
	m.Objects = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hogshead_objects", Help: "Current object versions."})
	m.Bytes = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hogshead_object_bytes", Help: "Logical bytes stored."})
	m.Buckets = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hogshead_buckets", Help: "Buckets."})
	m.Uploads = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hogshead_multipart_uploads", Help: "In-progress multipart uploads."})
	m.ChunksOnDisk = prometheus.NewGauge(prometheus.GaugeOpts{Name: "hogshead_chunks", Help: "Known chunks."})
	m.WorkerCounter = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "hogshead_worker_total", Help: "Background worker counters."}, []string{"counter"})
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.Requests, m.Duration, m.BytesIn, m.BytesOut, m.AuthFailures,
		m.Objects, m.Bytes, m.Buckets, m.Uploads, m.ChunksOnDisk, m.WorkerCounter)
	return m
}

// Handler serves the registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Observe records one request.
func (m *Metrics) Observe(op string, status int, dur time.Duration) {
	class := strconv.Itoa(status/100) + "xx"
	m.Requests.WithLabelValues(op, class).Inc()
	m.Duration.WithLabelValues(op).Observe(dur.Seconds())
}

// Sampler is polled periodically to refresh gauges.
type Sampler func(ctx context.Context, m *Metrics)

// Poll refreshes gauges every interval until ctx ends.
func (m *Metrics) Poll(ctx context.Context, interval time.Duration, s Sampler) {
	m.once.Do(func() {
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			s(ctx, m)
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					s(ctx, m)
				}
			}
		}()
	})
}

// TokenHandler wraps h so it requires a bearer token (header or ?token=).
// An empty token disables the endpoint.
func TokenHandler(token string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.Error(w, "metrics disabled; set metrics.token in the config", http.StatusNotFound)
			return
		}
		got := r.URL.Query().Get("token")
		if a := r.Header.Get("Authorization"); len(a) > 7 && a[:7] == "Bearer " {
			got = a[7:]
		}
		if got != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
