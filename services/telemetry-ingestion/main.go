// Command telemetry-ingestion accepts vehicle telemetry over REST and gRPC and writes
// it durably to Redis streams. It holds no state beyond in-flight requests, so it can
// be scaled horizontally without coordination.
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	telemetryv1 "github.com/ajtang/Intelletry/gen/telemetry/v1"
	"github.com/ajtang/Intelletry/internal/platform"
	"github.com/ajtang/Intelletry/internal/stream"
)

func main() {
	log := platform.NewLogger("telemetry-ingestion")
	ctx, stop := platform.SignalContext()
	defer stop()

	shutdownTracing, err := platform.InitTracing(ctx, "telemetry-ingestion")
	if err != nil {
		log.Error("tracing init failed", "error", err)
		os.Exit(1)
	}
	auth, err := NewAuthenticator(platform.MustEnv("INGEST_API_KEYS"))
	if err != nil {
		log.Error("invalid API keys", "error", err)
		os.Exit(1)
	}
	rdb, err := stream.NewClient(platform.MustEnv("REDIS_URL"))
	if err != nil {
		log.Error("invalid redis url", "error", err)
		os.Exit(1)
	}
	defer rdb.Close()

	reg := platform.NewRegistry()
	metrics := NewMetrics(reg)
	batcher := NewBatcher(BatcherConfig{
		Shards:       platform.EnvInt("STREAM_SHARDS", 16),
		Workers:      platform.EnvInt("BATCH_WORKERS", 8),
		QueueSize:    platform.EnvInt("BATCH_QUEUE_SIZE", 4096),
		MaxEvents:    platform.EnvInt("BATCH_MAX_EVENTS", 2000),
		Linger:       platform.EnvDuration("BATCH_LINGER", 10*time.Millisecond),
		MaxAttempts:  platform.EnvInt("PUBLISH_MAX_ATTEMPTS", 4),
		RetryBase:    100 * time.Millisecond,
		FlushTimeout: 2 * time.Second,
	}, &stream.Publisher{Client: rdb, MaxLen: int64(platform.EnvInt("STREAM_MAX_LEN", 100_000))}, metrics)

	ingestor := &Ingestor{
		limiter: NewLimiter(rdb, platform.EnvFloat("RATE_LIMIT_EPS", 20_000), platform.EnvFloat("RATE_LIMIT_BURST", 60_000)),
		batcher: batcher,
		metrics: metrics,
		now:     time.Now,
	}

	hc := platform.NewHealth()
	hc.Add("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() })
	mux := http.NewServeMux()
	hc.Register(mux, reg)
	mux.Handle("POST /telemetry", HTTPHandler(ingestor, auth))
	httpSrv := &http.Server{
		Addr:              ":" + platform.Env("HTTP_PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	grpcSrv := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxBodyBytes),
		grpc.UnaryInterceptor(UnaryAuth(auth, metrics)),
		grpc.StreamInterceptor(StreamAuth(auth, metrics)),
		// HTTP/2 connections are long lived, so without a maximum age new pods from an
		// HPA scale-out would receive no traffic from clients already connected.
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: 5 * time.Minute, MaxConnectionAgeGrace: 30 * time.Second}),
	)
	telemetryv1.RegisterTelemetryServiceServer(grpcSrv, &GRPCServer{ingestor: ingestor})
	grpcHealth := health.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, grpcHealth)

	lis, err := net.Listen("tcp", ":"+platform.Env("GRPC_PORT", "9090"))
	if err != nil {
		log.Error("grpc listen failed", "error", err)
		os.Exit(1)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("grpc listening", "addr", lis.Addr().String())
		if err := grpcSrv.Serve(lis); err != nil {
			log.Error("grpc server stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		hc.SetDraining()
		grpcHealth.Shutdown()
	}()

	if err := platform.Serve(ctx, httpSrv, platform.EnvDuration("SHUTDOWN_GRACE", 5*time.Second), log); err != nil {
		log.Error("http server failed", "error", err)
	}
	grpcSrv.GracefulStop()
	wg.Wait()
	batcher.Close() // only safe once both transports have stopped
	_ = shutdownTracing(context.Background())
	log.Info("shutdown complete")
}
