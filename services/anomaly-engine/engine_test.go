package main

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/ajtang/Intelletry/gen/telemetry/v1"
	"github.com/ajtang/Intelletry/internal/diag"
	"github.com/ajtang/Intelletry/internal/store"
	"github.com/ajtang/Intelletry/internal/stream"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	c, err := stream.NewClient(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.FlushDB(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	return c
}

func testDeps(t *testing.T, rdb *redis.Client, consumer string) *Deps {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.MaintainPartitions(ctx, time.Now(), 7); err != nil {
		t.Fatal(err)
	}
	return &Deps{
		RDB: rdb, Store: st, Analyzer: diag.NewAnalyzer(diag.Config{}), Fleet: NewFleetStats(rdb, 5),
		Metrics: NewMetrics(prometheus.NewRegistry()), Consumer: consumer, TimelineSamples: 10, SnapshotEvery: 50 * time.Millisecond,
	}
}

func publishEvents(t *testing.T, rdb *redis.Client, shard int, id string, seqs ...uint64) {
	t.Helper()
	batch := &telemetryv1.TelemetryBatch{}
	for _, seq := range seqs {
		batch.Events = append(batch.Events, &telemetryv1.TelemetryEvent{
			VehicleId: id, Seq: seq, TimestampMs: time.Now().UnixMilli(), SoftwareVersion: "1", Vin: "V", Model: "M", Region: "R",
			PackCapacityKwh: 80, SocPct: 50, BatteryTempC: 30, CoolantTempC: 30, AmbientTempC: 20, SignalDbm: -80,
			ChargingState: telemetryv1.ChargingState_CHARGING_STATE_NONE,
			Connectivity:  telemetryv1.ConnectivityState_CONNECTIVITY_STATE_ONLINE,
		})
	}
	raw, err := proto.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	pub := &stream.Publisher{Client: rdb, MaxLen: 1000}
	if err := pub.Publish(context.Background(), []stream.Entry{{Shard: shard, Payload: raw}}); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A consumer that crashes after reading but before acknowledging must lose nothing.
func TestCrashRecoveryIsLossless(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	deps := testDeps(t, rdb, "pod-b")
	key := stream.Key(0)
	if err := rdb.XGroupCreateMkStream(ctx, key, stream.Group, "0").Err(); err != nil {
		t.Fatal(err)
	}
	publishEvents(t, rdb, 0, "EV-1", 1, 2, 3)
	// pod-a reads the entry and dies without acknowledging it.
	if err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{Group: stream.Group, Consumer: "pod-a", Streams: []string{key, ">"}, Count: 10}).Err(); err != nil {
		t.Fatal(err)
	}
	publishEvents(t, rdb, 0, "EV-1", 4)

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		newShardWorker(0, deps).run(workerCtx)
	}()
	eventually(t, "all events processed", func() bool { return testutil.ToFloat64(deps.Metrics.events) == 4 })
	eventually(t, "pending list drained", func() bool {
		p, err := rdb.XPending(ctx, key, stream.Group).Result()
		return err == nil && p.Count == 0
	})
	cancel()
	<-done

	raw, err := rdb.Get(ctx, stream.StateKey("EV-1")).Bytes()
	if err != nil {
		t.Fatalf("state snapshot missing: %v", err)
	}
	var st diag.VehicleState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastSeq != 4 || st.LostSamples != 0 {
		t.Fatalf("resumed state: seq=%d lost=%d", st.LastSeq, st.LostSamples)
	}
	if n, _ := rdb.HLen(ctx, stream.FleetStateKey).Result(); n != 1 {
		t.Fatalf("fleet state entries = %d", n)
	}
	var vin string
	if err := deps.Store.Pool.QueryRow(ctx, `SELECT vin FROM vehicles WHERE vehicle_id='EV-1'`).Scan(&vin); err != nil || vin != "V" {
		t.Fatalf("vehicle not registered: %q %v", vin, err)
	}

	// A new owner resumes from the snapshot and treats a replay as a duplicate.
	publishEvents(t, rdb, 0, "EV-1", 4, 5)
	deps2 := &Deps{
		RDB: deps.RDB, Store: deps.Store, Analyzer: deps.Analyzer, Fleet: deps.Fleet, Consumer: "pod-c",
		Metrics: NewMetrics(prometheus.NewRegistry()), TimelineSamples: 10, SnapshotEvery: 50 * time.Millisecond,
	}
	workerCtx, cancel = context.WithCancel(ctx)
	defer cancel()
	done = make(chan struct{})
	go func() {
		defer close(done)
		newShardWorker(0, deps2).run(workerCtx)
	}()
	eventually(t, "replay handled", func() bool {
		return testutil.ToFloat64(deps2.Metrics.events) == 1 && testutil.ToFloat64(deps2.Metrics.duplicates) == 1
	})
	cancel()
	<-done
}

func TestPoisonEntryIsDeadLettered(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	deps := testDeps(t, rdb, "pod-a")
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream.Key(0), Values: map[string]any{stream.PayloadField: "\xff\xffgarbage"}}).Err(); err != nil {
		t.Fatal(err)
	}
	publishEvents(t, rdb, 0, "EV-1", 1)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		newShardWorker(0, deps).run(workerCtx)
	}()
	eventually(t, "shard to move past the poison entry", func() bool { return testutil.ToFloat64(deps.Metrics.events) == 1 })
	if n, _ := rdb.XLen(ctx, deadLetterKey).Result(); n != 1 {
		t.Fatalf("dead letter entries = %d", n)
	}
	cancel()
	<-done
}

func newTestCoordinator(rdb *redis.Client, id string, shards int) *Coordinator {
	return &Coordinator{
		rdb: rdb, id: id, shards: shards, leaseTTL: 600 * time.Millisecond, tick: 30 * time.Millisecond, owned: map[int]*ownedShard{},
		start: func(ctx context.Context, _ int) (<-chan struct{}, func()) {
			done := make(chan struct{})
			go func() {
				<-ctx.Done()
				close(done)
			}()
			return done, func() {}
		},
	}
}

// Shards must balance, never have two owners, and move on when a pod leaves.
func TestShardRebalancingAndFailover(t *testing.T) {
	rdb := testRedis(t)
	const shards = 16
	a, b := newTestCoordinator(rdb, "a", shards), newTestCoordinator(rdb, "b", shards)

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.Run(ctxA)
	}()
	eventually(t, "a to take every shard", func() bool { return a.OwnedCount() == shards })

	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		b.Run(ctxB)
	}()
	eventually(t, "even split", func() bool { return a.OwnedCount() == 8 && b.OwnedCount() == 8 })

	a.mu.Lock()
	for s := range a.owned {
		b.mu.Lock()
		if _, dup := b.owned[s]; dup {
			t.Errorf("shard %d owned by both pods", s)
		}
		b.mu.Unlock()
	}
	a.mu.Unlock()
	if a.IsLeader() == b.IsLeader() {
		t.Error("exactly one pod should hold the leader shard")
	}

	cancelA() // graceful exit releases leases at once
	wg.Wait()
	eventually(t, "b to absorb all shards", func() bool { return b.OwnedCount() == shards })
	if !b.IsLeader() {
		t.Error("b should have inherited leadership")
	}
	cancelB()
	<-doneB
	if n, _ := rdb.Exists(context.Background(), leaseKey(0)).Result(); n != 0 {
		t.Error("graceful shutdown should release leases")
	}
}

// A pod that vanishes without releasing anything loses its shards when leases expire.
func TestLeaseExpiryAfterCrash(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	for s := 0; s < 4; s++ {
		if err := rdb.Set(ctx, leaseKey(s), "dead-pod", 300*time.Millisecond).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// The dead pod is still listed as a member, as it would be right after a SIGKILL.
	if err := rdb.ZAdd(ctx, membersKey, redis.Z{Score: float64(time.Now().UnixMilli()), Member: "dead-pod"}).Err(); err != nil {
		t.Fatal(err)
	}
	c := newTestCoordinator(rdb, "survivor", 4)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(runCtx)
	}()
	eventually(t, "survivor to take expired leases", func() bool { return c.OwnedCount() == 4 })
	cancel()
	<-done
}

func TestFleetStatsMergeAcrossPods(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	now := time.Now()
	podA, podB := NewFleetStats(rdb, 5), NewFleetStats(rdb, 5)
	for i := 0; i < 30; i++ {
		podA.Add([]diag.Sample{{Metric: diag.MetricBatteryTemp, Cohort: "dc|a20", Value: 30}})
		podB.Add([]diag.Sample{{Metric: diag.MetricBatteryTemp, Cohort: "dc|a20", Value: 30}})
	}
	if err := podA.Flush(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := podB.Flush(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := podA.Refresh(ctx, now); err != nil {
		t.Fatal(err)
	}
	st, ok := podA.Current().Lookup(diag.MetricBatteryTemp, "dc|a20")
	if !ok || st.N != 60 {
		t.Fatalf("pod A should see the whole fleet: %+v ok=%v", st, ok)
	}
}

func TestRegressionTrackerHysteresis(t *testing.T) {
	r := newRegressionTracker()
	now := time.Now()
	finding := []diag.VersionFinding{{Version: "v2", Metric: diag.MetricIdleKW, Median: 1, PeerMedian: 0.05, EffectPct: 1900, Samples: 500, PeerSamples: 500}}
	for i := 0; i < regressionConfirm-1; i++ {
		if got := r.observe(finding, now); len(got) != 0 {
			t.Fatalf("opened too early: %+v", got)
		}
	}
	got := r.observe(finding, now)
	if len(got) != 1 || got[0].Transition != diag.TransitionOpened || got[0].Subject != "software:v2" {
		t.Fatalf("expected open: %+v", got)
	}
	r.rollback(got)
	if got = r.observe(finding, now); len(got) != 1 {
		t.Fatal("a failed save should be retried")
	}
	for i := 0; i < regressionClear-1; i++ {
		if got := r.observe(nil, now); len(got) != 0 {
			t.Fatalf("cleared too early: %+v", got)
		}
	}
	if got = r.observe(nil, now); len(got) != 1 || got[0].Transition != diag.TransitionCleared {
		t.Fatalf("expected clear: %+v", got)
	}
}
