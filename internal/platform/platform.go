// Package platform provides the operational baseline every service shares:
// configuration, structured logging, metrics, tracing and health endpoints.
package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Env returns an environment variable or a default.
func Env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// EnvInt returns an integer environment variable or a default.
func EnvInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// EnvFloat returns a float environment variable or a default.
func EnvFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

// EnvDuration returns a duration environment variable or a default.
func EnvDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// MustEnv returns a required environment variable, or exits with a clear error.
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required environment variable", "name", key)
		os.Exit(1)
	}
	return v
}

// NewLogger installs a JSON logger on stdout, which Azure Monitor Container Insights
// collects without any extra agent configuration.
func NewLogger(service string) *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	l := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).With("service", service)
	slog.SetDefault(l)
	return l
}

// SignalContext is cancelled on SIGINT or SIGTERM.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// InitTracing configures OTLP export when OTEL_EXPORTER_OTLP_ENDPOINT is set. In AKS
// this points to a collector that forwards to Application Insights. Propagation is
// configured either way, so trace context always flows across services.
func InitTracing(ctx context.Context, service string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res := resource.NewSchemaless(semconv.ServiceName(service))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(EnvFloat("TRACE_SAMPLE_RATIO", 0.05)))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Check is a named dependency check.
type Check func(ctx context.Context) error

// Health tracks liveness and readiness.
//
// Liveness intentionally ignores dependencies. If Redis is down, restarting every pod
// would not fix Redis and would add a restart storm to the outage. Readiness is what
// takes a pod out of rotation.
type Health struct {
	mu       sync.RWMutex
	checks   map[string]Check
	draining bool
}

// NewHealth returns an empty registry.
func NewHealth() *Health { return &Health{checks: map[string]Check{}} }

// Add registers a readiness check.
func (h *Health) Add(name string, c Check) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[name] = c
}

// SetDraining fails readiness so load balancers stop sending traffic before shutdown.
func (h *Health) SetDraining() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.draining = true
}

func (h *Health) ready(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.draining {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	for name, c := range h.checks {
		if err := c(ctx); err != nil {
			http.Error(w, name+": "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	_, _ = w.Write([]byte("ok"))
}

// Register adds /healthz, /readyz and /metrics to a mux.
func (h *Health) Register(mux *http.ServeMux, reg *prometheus.Registry) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", h.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
}

// NewRegistry returns a registry with runtime collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Serve runs an HTTP server until ctx ends, then drains. The readiness grace period
// lets Kubernetes endpoints update before connections start being refused.
func Serve(ctx context.Context, srv *http.Server, grace time.Duration, log *slog.Logger) error {
	errc := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	time.Sleep(grace)
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
