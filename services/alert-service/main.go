// Command alert-service turns diagnostics into operational alerts with a managed
// lifecycle, and serves the operations API and real-time feed for the fleet dashboard.
package main

import (
	"context"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ajtang/Intelletry/internal/platform"
	"github.com/ajtang/Intelletry/internal/store"
	"github.com/ajtang/Intelletry/internal/stream"
)

func main() {
	log := platform.NewLogger("alert-service")
	ctx, stop := platform.SignalContext()
	defer stop()

	shutdownTracing, err := platform.InitTracing(ctx, "alert-service")
	if err != nil {
		log.Error("tracing init failed", "error", err)
		os.Exit(1)
	}
	token := platform.MustEnv("API_TOKEN")
	if len(token) < 16 {
		log.Error("API_TOKEN must be at least 16 characters")
		os.Exit(1)
	}
	rdb, err := stream.NewClient(platform.MustEnv("REDIS_URL"))
	if err != nil {
		log.Error("invalid redis url", "error", err)
		os.Exit(1)
	}
	defer rdb.Close()
	st, err := store.Open(ctx, platform.MustEnv("DATABASE_URL"), int32(platform.EnvInt("DB_MAX_CONNS", 10)))
	if err != nil {
		log.Error("postgres connection failed", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}
	streamShards = platform.EnvInt("STREAM_SHARDS", 16)

	reg := platform.NewRegistry()
	created := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "alerts_created_total", Help: "Alerts created."}, []string{"level", "category"})
	resolved := prometheus.NewCounter(prometheus.CounterOpts{Name: "alerts_auto_resolved_total", Help: "Alerts resolved by the engine."})
	outbox := prometheus.NewGauge(prometheus.GaugeOpts{Name: "alerts_outbox_depth", Help: "Diagnostics waiting to be processed."})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "api_request_duration_seconds", Help: "API latency.", Buckets: prometheus.DefBuckets}, []string{"route", "code"})
	clients := prometheus.NewGauge(prometheus.GaugeOpts{Name: "api_sse_clients", Help: "Connected operations feed clients."})
	online := prometheus.NewGauge(prometheus.GaugeOpts{Name: "fleet_vehicles_online", Help: "Vehicles reporting recently."})
	total := prometheus.NewGauge(prometheus.GaugeOpts{Name: "fleet_vehicles_total", Help: "Vehicles with live state."})
	reg.MustRegister(created, resolved, outbox, duration, clients, online, total)

	hub := NewHub(clients)
	api := &API{st: st, rdb: rdb, token: token, hub: hub, duration: duration, online: online, total: total,
		offlineAfter: platform.EnvDuration("OFFLINE_AFTER", 30*time.Second)}
	processor := &Processor{st: st, rdb: rdb, created: created, resolved: resolved, lag: outbox}

	hc := platform.NewHealth()
	hc.Add("postgres", st.Ping)
	hc.Add("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() })
	mux := http.NewServeMux()
	hc.Register(mux, reg)
	api.Routes(mux)
	srv := &http.Server{
		Addr:              ":" + platform.Env("HTTP_PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	// Streams never finish on their own, so they must be told to end or Shutdown would
	// wait out its whole timeout. Ordinary requests are left to complete.
	api.stopStreams = make(chan struct{})
	srv.RegisterOnShutdown(func() { close(api.stopStreams) })

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		processor.Run(ctx, platform.EnvDuration("PROCESS_INTERVAL", 500*time.Millisecond))
	}()
	go func() {
		defer wg.Done()
		hub.Run(ctx, rdb)
	}()
	go func() {
		<-ctx.Done()
		hc.SetDraining()
	}()
	if err := platform.Serve(ctx, srv, platform.EnvDuration("SHUTDOWN_GRACE", 5*time.Second), log); err != nil {
		log.Error("http server failed", "error", err)
	}
	wg.Wait()
	_ = shutdownTracing(context.Background())
	log.Info("shutdown complete")
}
