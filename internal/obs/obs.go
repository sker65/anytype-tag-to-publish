// Package obs holds metrics and the internal health/metrics HTTP server.
package obs

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry         *prometheus.Registry
	PublishLatency   prometheus.Histogram
	Events           *prometheus.CounterVec
	StreamReconnects prometheus.Counter
	PublishedObjects prometheus.Gauge
	Renders          *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		PublishLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "anypub_publish_latency_seconds",
			Help:    "Time from receiving the event to the public URL being written back.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 3, 5, 10, 30},
		}),
		Events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "anypub_events_total",
			Help: "Stream events received, by type.",
		}, []string{"type"}),
		StreamReconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "anypub_stream_reconnects_total",
			Help: "Search stream reconnects.",
		}),
		PublishedObjects: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "anypub_published_objects",
			Help: "Objects currently published.",
		}),
		Renders: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "anypub_renders_total",
			Help: "Processing outcomes: written, unchanged, skipped_self, skipped_unmodified, error.",
		}, []string{"result"}),
	}
	m.Registry.MustRegister(m.PublishLatency, m.Events, m.StreamReconnects, m.PublishedObjects, m.Renders,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

// Serve runs /healthz, /readyz and /metrics until ctx ends.
func Serve(ctx context.Context, addr string, m *Metrics, ready func() bool) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "streams not ready\n", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
