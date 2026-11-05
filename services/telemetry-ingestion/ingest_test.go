package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/intelletry/ev-telemetry/gen/telemetry/v1"
	"github.com/intelletry/ev-telemetry/internal/stream"
)

type fakePublisher struct {
	calls    atomic.Int64
	events   atomic.Int64
	failures atomic.Int64
	block    chan struct{}
}

func (f *fakePublisher) Publish(_ context.Context, entries []stream.Entry) error {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	if f.failures.Add(-1) >= 0 {
		return errors.New("redis down")
	}
	for _, e := range entries {
		var b telemetryv1.TelemetryBatch
		if err := proto.Unmarshal(e.Payload, &b); err != nil {
			return err
		}
		for _, ev := range b.Events {
			if stream.ShardFor(ev.VehicleId, 4) != e.Shard {
				return errors.New("event routed to wrong shard")
			}
		}
		f.events.Add(int64(len(b.Events)))
	}
	return nil
}

func testBatcher(pub publisher, queue int) *Batcher {
	return NewBatcher(BatcherConfig{
		Shards: 4, Workers: 2, QueueSize: queue, MaxEvents: 100, Linger: 5 * time.Millisecond,
		MaxAttempts: 3, RetryBase: time.Millisecond, FlushTimeout: time.Second,
	}, pub, NewMetrics(prometheus.NewRegistry()))
}

func event(id string, seq uint64) *telemetryv1.TelemetryEvent {
	return &telemetryv1.TelemetryEvent{
		VehicleId: id, Seq: seq, TimestampMs: time.Now().UnixMilli(), SoftwareVersion: "1", SocPct: 50,
		PackCapacityKwh: 80,
		ChargingState:   telemetryv1.ChargingState_CHARGING_STATE_NONE,
		Connectivity:    telemetryv1.ConnectivityState_CONNECTIVITY_STATE_ONLINE,
	}
}

func TestBatcherGroupCommit(t *testing.T) {
	pub := &fakePublisher{}
	b := testBatcher(pub, 1000)
	done := make(chan error, 200)
	for i := 0; i < 200; i++ {
		go func() {
			done <- b.Submit(context.Background(), []*telemetryv1.TelemetryEvent{event("EV-1", uint64(i+1))}, "")
		}()
	}
	for i := 0; i < 200; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b.Close()
	if pub.events.Load() != 200 {
		t.Fatalf("stored %d events, want 200", pub.events.Load())
	}
	if pub.calls.Load() >= 200 {
		t.Fatalf("group commit did not batch: %d publishes for 200 events", pub.calls.Load())
	}
}

func TestBatcherRetriesThenSucceeds(t *testing.T) {
	pub := &fakePublisher{}
	pub.failures.Store(2)
	b := testBatcher(pub, 10)
	defer b.Close()
	if err := b.Submit(context.Background(), []*telemetryv1.TelemetryEvent{event("EV-1", 1)}, ""); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if pub.calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", pub.calls.Load())
	}
}

func TestBatcherReportsPersistentFailure(t *testing.T) {
	pub := &fakePublisher{}
	pub.failures.Store(100)
	b := testBatcher(pub, 10)
	defer b.Close()
	err := b.Submit(context.Background(), []*telemetryv1.TelemetryEvent{event("EV-1", 1)}, "")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestBatcherAppliesBackpressure(t *testing.T) {
	pub := &fakePublisher{block: make(chan struct{})}
	b := testBatcher(pub, 1)
	overloaded := make(chan struct{})
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			err := b.Submit(ctx, []*telemetryv1.TelemetryEvent{event("EV-1", 1)}, "")
			cancel()
			if errors.Is(err, ErrOverloaded) {
				close(overloaded)
				return
			}
		}
	}()
	select {
	case <-overloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("queue never reported overload")
	}
	close(pub.block)
	b.Close()
}

func TestAuthenticator(t *testing.T) {
	if _, err := NewAuthenticator("gw:short"); err == nil {
		t.Fatal("short secret should be refused")
	}
	a, err := NewAuthenticator("gw-a:aaaaaaaaaaaaaaaaaaaa,gw-b:bbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := a.Authenticate("Bearer bbbbbbbbbbbbbbbbbbbb"); !ok || id != "gw-b" {
		t.Fatalf("got %q %v", id, ok)
	}
	for _, h := range []string{"", "Bearer nope", "bbbbbbbbbbbbbbbbbbbb", "Basic bbbbbbbbbbbbbbbbbbbb"} {
		if _, ok := a.Authenticate(h); ok {
			t.Fatalf("%q should not authenticate", h)
		}
	}
}

func TestLocalFallbackLimiter(t *testing.T) {
	// A closed client makes every Redis call fail, as during an outage.
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 5 * time.Millisecond})
	defer c.Close()
	l := NewLimiter(c, 1, 10)
	if !l.Allow(context.Background(), "gw", 10) {
		t.Fatal("first batch should use the burst allowance")
	}
	if l.Allow(context.Background(), "gw", 10) {
		t.Fatal("second batch should exceed the local allowance")
	}
}

func testServer(t *testing.T, pub publisher) http.Handler {
	t.Helper()
	auth, err := NewAuthenticator("gw:secretsecretsecret1")
	if err != nil {
		t.Fatal(err)
	}
	m := NewMetrics(prometheus.NewRegistry())
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 5 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close() })
	b := NewBatcher(BatcherConfig{Shards: 4, Workers: 1, QueueSize: 10, MaxEvents: 10, Linger: time.Millisecond, MaxAttempts: 1, RetryBase: time.Millisecond, FlushTimeout: time.Second}, pub, m)
	t.Cleanup(b.Close)
	return HTTPHandler(&Ingestor{limiter: NewLimiter(c, 1000, 1000), batcher: b, metrics: m, now: time.Now}, auth)
}

func TestHTTPStatusCodes(t *testing.T) {
	now := time.Now().UnixMilli()
	valid := `{"vehicle_id":"EV-1","seq":1,"timestamp_ms":` + itoa(now) + `,"software_version":"1","soc_pct":50,"pack_capacity_kwh":80,"charging_state":"CHARGING_STATE_NONE","connectivity":"CONNECTIVITY_STATE_ONLINE"}`
	bad := strings.Replace(valid, `"soc_pct":50`, `"soc_pct":150`, 1)
	cases := []struct {
		name, auth, body string
		want             int
		failures         int64
	}{
		{"no auth", "", valid, 401, 0},
		{"single event", "Bearer secretsecretsecret1", valid, 202, 0},
		{"batch", "Bearer secretsecretsecret1", `{"events":[` + valid + `]}`, 202, 0},
		{"partial accept", "Bearer secretsecretsecret1", `{"events":[` + valid + `,` + bad + `]}`, 202, 0},
		{"all invalid", "Bearer secretsecretsecret1", `{"events":[` + bad + `]}`, 422, 0},
		{"malformed", "Bearer secretsecretsecret1", `{not json`, 400, 0},
		{"empty", "Bearer secretsecretsecret1", `{"events":[]}`, 400, 0},
		{"redis down", "Bearer secretsecretsecret1", valid, 503, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &fakePublisher{}
			pub.failures.Store(tc.failures)
			h := testServer(t, pub)
			req := httptest.NewRequest("POST", "/telemetry", strings.NewReader(tc.body))
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 503 && rec.Header().Get("Retry-After") == "" {
				t.Fatal("503 should carry Retry-After")
			}
		})
	}
}

func itoa(v int64) string {
	b := make([]byte, 0, 20)
	if v == 0 {
		return "0"
	}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
