// Command vehicle-simulator drives the physical fleet model and sends telemetry to the
// ingestion service, behaving as a well-mannered vehicle gateway: it batches, retries
// with backoff, honours backpressure and buffers a bounded amount during outages.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/ajtang/Intelletry/gen/telemetry/v1"
	"github.com/ajtang/Intelletry/internal/platform"
	"github.com/ajtang/Intelletry/internal/sim"
)

type sender interface {
	Send(ctx context.Context, batch *telemetryv1.TelemetryBatch) error
}

type httpSender struct {
	url, token string
	client     *http.Client
}

func (s *httpSender) Send(ctx context.Context, batch *telemetryv1.TelemetryBatch) error {
	body, err := proto.Marshal(batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/telemetry", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("ingestion returned %d", resp.StatusCode)
	}
	return nil
}

type grpcSender struct {
	client telemetryv1.TelemetryServiceClient
	token  string
}

func (s *grpcSender) Send(ctx context.Context, batch *telemetryv1.TelemetryBatch) error {
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+s.token)
	_, err := s.client.IngestBatch(ctx, batch)
	return err
}

type metrics struct {
	sent, dropped, failed prometheus.Counter
	vehicles, backlog     prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		sent:     prometheus.NewCounter(prometheus.CounterOpts{Name: "sim_events_sent_total", Help: "Events acknowledged by ingestion."}),
		dropped:  prometheus.NewCounter(prometheus.CounterOpts{Name: "sim_events_dropped_total", Help: "Events dropped because the bounded buffer overflowed."}),
		failed:   prometheus.NewCounter(prometheus.CounterOpts{Name: "sim_send_failures_total", Help: "Failed send attempts."}),
		vehicles: prometheus.NewGauge(prometheus.GaugeOpts{Name: "sim_vehicles", Help: "Simulated vehicles."}),
		backlog:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "sim_backlog_events", Help: "Events waiting to be sent."}),
	}
	reg.MustRegister(m.sent, m.dropped, m.failed, m.vehicles, m.backlog)
	return m
}

// buffer is a bounded FIFO. When full it drops the oldest events: during a long outage
// the newest data is the most useful, as with real vehicle store-and-forward.
type buffer struct {
	mu     sync.Mutex
	events []*telemetryv1.TelemetryEvent
	max    int
	m      *metrics
}

func (b *buffer) add(evs []*telemetryv1.TelemetryEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, evs...)
	if over := len(b.events) - b.max; over > 0 {
		b.events = b.events[over:]
		b.m.dropped.Add(float64(over))
	}
	b.m.backlog.Set(float64(len(b.events)))
}

func (b *buffer) take(n int) []*telemetryv1.TelemetryEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > len(b.events) {
		n = len(b.events)
	}
	out := b.events[:n:n]
	b.events = b.events[n:]
	b.m.backlog.Set(float64(len(b.events)))
	return out
}

func sendLoop(ctx context.Context, s sender, b *buffer, batchSize int, m *metrics) {
	backoff := 100 * time.Millisecond
	for {
		events := b.take(batchSize)
		if len(events) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}
		for {
			sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.Send(sendCtx, &telemetryv1.TelemetryBatch{Events: events})
			cancel()
			if err == nil {
				m.sent.Add(float64(len(events)))
				backoff = 100 * time.Millisecond
				break
			}
			m.failed.Inc()
			slog.Warn("send failed", "error", err, "events", len(events), "backoff_ms", backoff.Milliseconds())
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(rand.Int63n(int64(backoff))) + backoff/2):
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
		}
	}
}

func main() {
	log := platform.NewLogger("vehicle-simulator")
	ctx, stop := platform.SignalContext()
	defer stop()

	vehicles := platform.EnvInt("SIM_VEHICLES", 100)
	interval := platform.EnvDuration("SIM_INTERVAL", 2*time.Second)
	timeScale := platform.EnvFloat("SIM_TIME_SCALE", 60)
	faultRate := platform.EnvFloat("SIM_FAULT_RATE", 0.10)
	token := platform.MustEnv("INGEST_API_KEY")
	transport := platform.Env("SIM_TRANSPORT", "grpc")
	seed := int64(platform.EnvInt("SIM_SEED", 1))

	var s sender
	switch transport {
	case "grpc":
		conn, err := grpc.NewClient(platform.Env("INGEST_GRPC_ADDR", "localhost:9090"),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`))
		if err != nil {
			log.Error("grpc client failed", "error", err)
			os.Exit(1)
		}
		defer conn.Close()
		s = &grpcSender{client: telemetryv1.NewTelemetryServiceClient(conn), token: token}
	case "http":
		s = &httpSender{url: platform.Env("INGEST_HTTP_URL", "http://localhost:8080"), token: token, client: &http.Client{Timeout: 15 * time.Second}}
	default:
		log.Error("SIM_TRANSPORT must be grpc or http", "value", transport)
		os.Exit(1)
	}

	start := time.Now()
	fleet := sim.NewFleet(sim.FleetConfig{
		Vehicles: vehicles, Seed: seed, Start: start, FaultRate: faultRate,
		MaxOnset: time.Duration(float64(5*time.Minute) * timeScale),
	})

	reg := platform.NewRegistry()
	m := newMetrics(reg)
	m.vehicles.Set(float64(vehicles))
	hc := platform.NewHealth()
	mux := http.NewServeMux()
	hc.Register(mux, reg)
	// Ground truth for evaluation. A real fleet has no such endpoint, and the engine
	// never reads it.
	mux.HandleFunc("GET /faults", func(w http.ResponseWriter, _ *http.Request) {
		out := map[string]string{}
		for _, v := range fleet {
			if v.Fault != sim.FaultNone {
				out[v.ID] = string(v.Fault)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	go func() {
		if err := platform.Serve(ctx, &http.Server{Addr: ":" + platform.Env("HTTP_PORT", "8081"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}, 0, log); err != nil {
			log.Error("metrics server failed", "error", err)
		}
	}()

	buf := &buffer{max: platform.EnvInt("SIM_BUFFER_EVENTS", vehicles*60), m: m}
	batchSize := platform.EnvInt("SIM_BATCH_SIZE", 500)
	var wg sync.WaitGroup
	for i := 0; i < platform.EnvInt("SIM_SENDERS", 4); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendLoop(ctx, s, buf, batchSize, m)
		}()
	}

	// Vehicles report on staggered slots so the fleet generates a smooth load rather
	// than a synchronized spike every interval.
	const slots = 20
	slotTick := interval / slots
	vehicleDt := time.Duration(float64(interval) * timeScale)
	vclock := make([]time.Time, slots)
	for i := range vclock {
		vclock[i] = start
	}
	log.Info("simulation started", "vehicles", vehicles, "interval", interval.String(), "time_scale", timeScale,
		"events_per_sec", float64(vehicles)/interval.Seconds(), "transport", transport)

	ticker := time.NewTicker(slotTick)
	defer ticker.Stop()
	slot := 0
	for {
		select {
		case <-ctx.Done():
			hc.SetDraining()
			wg.Wait()
			log.Info("shutdown complete", "buffered_events", len(buf.events))
			return
		case now := <-ticker.C:
			vclock[slot] = vclock[slot].Add(vehicleDt)
			out := make([]*telemetryv1.TelemetryEvent, 0, vehicles/slots+1)
			for i := slot; i < len(fleet); i += slots {
				if ev := fleet[i].Step(vclock[slot], vehicleDt); ev != nil {
					ev.TimestampMs = now.UnixMilli() // on-wire time is real; physics time is accelerated
					out = append(out, ev)
				}
			}
			buf.add(out)
			slot = (slot + 1) % slots
		}
	}
}
