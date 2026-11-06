package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/ajtang/Intelletry/gen/telemetry/v1"
	"github.com/ajtang/Intelletry/internal/diag"
	"github.com/ajtang/Intelletry/internal/store"
	"github.com/ajtang/Intelletry/internal/stream"
	"github.com/ajtang/Intelletry/internal/telemetry"
)

const deadLetterKey = "telemetry:dlq"

// Metrics are the anomaly engine's Prometheus series.
type Metrics struct {
	events      prometheus.Counter
	duplicates  prometheus.Counter
	poison      prometheus.Counter
	diagnostics *prometheus.CounterVec
	batch       prometheus.Histogram
	lag         prometheus.Histogram
	depth       *prometheus.GaugeVec
	owned       prometheus.Gauge
	vehicles    prometheus.Gauge
	errors      *prometheus.CounterVec
}

// NewMetrics registers the series.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		events:      prometheus.NewCounter(prometheus.CounterOpts{Name: "engine_events_processed_total", Help: "Telemetry events analyzed."}),
		duplicates:  prometheus.NewCounter(prometheus.CounterOpts{Name: "engine_events_duplicate_total", Help: "Events dropped as duplicate or stale."}),
		poison:      prometheus.NewCounter(prometheus.CounterOpts{Name: "engine_entries_dead_lettered_total", Help: "Undecodable stream entries."}),
		diagnostics: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "engine_diagnostics_total", Help: "Diagnostic transitions."}, []string{"category", "transition"}),
		batch:       prometheus.NewHistogram(prometheus.HistogramOpts{Name: "engine_batch_duration_seconds", Help: "Time to process one read.", Buckets: prometheus.DefBuckets}),
		lag:         prometheus.NewHistogram(prometheus.HistogramOpts{Name: "engine_event_lag_seconds", Help: "Vehicle timestamp to analysis.", Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300}}),
		depth:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "engine_queue_depth", Help: "Entries waiting per shard."}, []string{"shard"}),
		owned:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "engine_owned_shards", Help: "Shards held by this pod."}),
		vehicles:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "engine_vehicles_tracked", Help: "Vehicles in memory on this pod."}),
		errors:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "engine_errors_total", Help: "Runtime errors."}, []string{"stage"}),
	}
	reg.MustRegister(m.events, m.duplicates, m.poison, m.diagnostics, m.batch, m.lag, m.depth, m.owned, m.vehicles, m.errors)
	return m
}

// Deps are the resources shared by shard workers.
type Deps struct {
	RDB             *redis.Client
	Store           *store.Store
	Analyzer        *diag.Analyzer
	Fleet           *FleetStats
	Metrics         *Metrics
	Consumer        string
	TimelineSamples int64
	SnapshotEvery   time.Duration
	tracked         atomic.Int64
}

type vehicleEntry struct {
	state    *diag.VehicleState
	dirty    bool
	software string
	known    bool
}

type rollupAcc struct {
	store.Rollup
	residualSum float64
	residualN   int
}

// shardWorker owns one stream shard. All vehicle state is touched by one goroutine, so
// analysis needs no locks.
type shardWorker struct {
	shard    int
	d        *Deps
	vehicles map[string]*vehicleEntry
	rollups  map[string]*rollupAcc
	// fenced is set when snapshots must not be written, either because the lease was
	// lost or because in-memory state has outrun what was durably saved.
	fenced atomic.Bool
}

func newShardWorker(shard int, d *Deps) *shardWorker {
	return &shardWorker{shard: shard, d: d, vehicles: map[string]*vehicleEntry{}, rollups: map[string]*rollupAcc{}}
}

func (w *shardWorker) run(ctx context.Context) {
	key := stream.Key(w.shard)
	defer func() {
		w.d.tracked.Add(-int64(len(w.vehicles)))
		w.d.Metrics.vehicles.Set(float64(w.d.tracked.Load()))
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		w.flushRollups(shutdown, time.Now().Add(time.Hour))
		if !w.fenced.Load() {
			w.snapshot(shutdown)
		}
	}()

	for ctx.Err() == nil {
		err := w.d.RDB.XGroupCreateMkStream(ctx, key, stream.Group, "0").Err()
		if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
			break
		}
		w.d.Metrics.errors.WithLabelValues("group_create").Inc()
		sleep(ctx, time.Second)
	}
	w.recoverPending(ctx, key)

	lastSnapshot := time.Now()
	lastDepth := time.Time{}
	for ctx.Err() == nil {
		streams, err := w.d.RDB.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: stream.Group, Consumer: w.d.Consumer, Streams: []string{key, ">"}, Count: 64, Block: time.Second,
		}).Result()
		switch {
		case errors.Is(err, redis.Nil):
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			if strings.Contains(err.Error(), "NOGROUP") {
				// The stream vanished, for example after a Redis failover without persistence.
				_ = w.d.RDB.XGroupCreateMkStream(ctx, key, stream.Group, "0").Err()
			}
			w.d.Metrics.errors.WithLabelValues("read").Inc()
			sleep(ctx, 500*time.Millisecond)
			continue
		default:
			for _, s := range streams {
				if !w.process(ctx, key, s.Messages) {
					return
				}
			}
		}
		now := time.Now()
		if ctx.Err() != nil {
			return // the deferred handoff writes the final snapshot
		}
		if now.Sub(lastSnapshot) >= w.d.SnapshotEvery {
			w.snapshot(ctx)
			w.flushRollups(ctx, now)
			lastSnapshot = now
		}
		if now.Sub(lastDepth) >= 5*time.Second {
			if depth, err := stream.Depth(ctx, w.d.RDB, w.shard); err == nil {
				w.d.Metrics.depth.WithLabelValues(key[strings.LastIndex(key, ":")+1:]).Set(float64(depth))
			}
			lastDepth = now
		}
	}
}

// recoverPending takes over entries delivered to a previous owner but never acknowledged,
// which is what makes a crash mid-batch lossless. Holding the lease makes this safe.
func (w *shardWorker) recoverPending(ctx context.Context, key string) {
	cursor := "0-0"
	for ctx.Err() == nil {
		msgs, next, err := w.d.RDB.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: key, Group: stream.Group, Consumer: w.d.Consumer, MinIdle: 0, Start: cursor, Count: 64,
		}).Result()
		if err != nil {
			w.d.Metrics.errors.WithLabelValues("autoclaim").Inc()
			return
		}
		if len(msgs) > 0 {
			slog.Info("recovering pending entries", "shard", w.shard, "count", len(msgs))
			if !w.process(ctx, key, msgs) {
				return
			}
		}
		if next == "0-0" || next == "" {
			return
		}
		cursor = next
	}
}

// process analyzes messages and acknowledges them only after their effects are stored,
// giving at-least-once delivery. Sequence checks make replays harmless. It returns false
// if the worker must stop.
func (w *shardWorker) process(ctx context.Context, key string, msgs []redis.XMessage) bool {
	start := time.Now()
	defer func() { w.d.Metrics.batch.Observe(time.Since(start).Seconds()) }()

	var (
		ids         = make([]string, 0, len(msgs))
		diagnoses   []diag.Diagnosis
		newVehicles []store.VehicleMeta
		latest      = map[string][]byte{}
		timeline    = map[string][]any{}
		live        []liveMessage
		processed   int64
	)
	snap := w.d.Fleet.Current()
	history := w.d.Fleet.History()
	for _, msg := range msgs {
		ids = append(ids, msg.ID)
		payload, _ := msg.Values[stream.PayloadField].(string)
		batch := &telemetryv1.TelemetryBatch{}
		if err := proto.Unmarshal([]byte(payload), batch); err != nil {
			// A poison entry must not block the shard forever.
			w.d.Metrics.poison.Inc()
			_ = w.d.RDB.XAdd(ctx, &redis.XAddArgs{Stream: deadLetterKey, MaxLen: 10_000, Approx: true, Values: msg.Values}).Err()
			slog.Error("dead-lettered undecodable entry", "shard", w.shard, "id", msg.ID, "error", err)
			continue
		}
		traceParent, _ := msg.Values[stream.TraceField].(string)
		spanCtx := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": traceParent})
		_, span := otel.Tracer("anomaly-engine").Start(spanCtx, "engine.process_entry")

		for _, ev := range batch.Events {
			entry, err := w.entry(ctx, ev.VehicleId)
			if err != nil {
				span.End()
				w.d.Metrics.errors.WithLabelValues("state_load").Inc()
				return ctx.Err() == nil // unacknowledged, so it will be retried
			}
			if h, ok := history[ev.VehicleId]; ok {
				entry.state.SetHistory(h)
			}
			res := w.d.Analyzer.Process(entry.state, ev, snap)
			if res.Duplicate {
				w.d.Metrics.duplicates.Inc()
				continue
			}
			processed++
			entry.dirty = true
			w.d.Fleet.Add(res.Samples)
			diagnoses = append(diagnoses, res.Changes...)
			if !entry.known || entry.software != ev.SoftwareVersion {
				entry.known, entry.software = true, ev.SoftwareVersion
				newVehicles = append(newVehicles, store.VehicleMeta{
					VehicleID: ev.VehicleId, VIN: ev.Vin, Model: ev.Model, Region: ev.Region,
					SoftwareVersion: ev.SoftwareVersion, PackCapacityKWh: ev.PackCapacityKwh,
				})
			}
			state := buildLive(ev, res.Features)
			encoded, err := json.Marshal(state)
			if err == nil {
				latest[ev.VehicleId] = encoded
				live = append(live, liveMessage{ev.VehicleId, encoded})
			}
			point, _ := json.Marshal(telemetry.TimelineSample{
				T: ev.TimestampMs, Battery: ev.BatteryTempC, Motor: ev.MotorTempC, Ambient: ev.AmbientTempC,
				SoC: ev.SocPct, Speed: ev.SpeedKph, Power: ev.PackPowerKw,
			})
			timeline[ev.VehicleId] = append(timeline[ev.VehicleId], point)
			w.addRollup(ev, res.Features)
			w.d.Metrics.lag.Observe(time.Since(time.UnixMilli(ev.TimestampMs)).Seconds())
		}
		span.End()
	}

	// Order matters. Diagnostics are stored first and retried until they succeed, so a
	// detection cannot be acknowledged away. If they cannot be stored before shutdown,
	// the worker is fenced: the new owner resumes from the last snapshot, reprocesses the
	// unacknowledged entries, and detects the same transitions again.
	if err := w.saveDurable(ctx, newVehicles, diagnoses); err != nil {
		w.fenced.Store(true)
		return false
	}
	for _, d := range diagnoses {
		w.d.Metrics.diagnostics.WithLabelValues(string(d.Category), string(d.Transition)).Inc()
		slog.Info("diagnosis", "subject", d.Subject, "category", d.Category, "transition", d.Transition,
			"severity", d.Severity, "confidence", d.Confidence)
	}

	// From here the batch finishes even if the shard is being handed off, because abandoning
	// it now would only force the next owner to redeliver work that is already complete.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	// Live state is a best-effort cache, rebuilt by the next sample, so failing to write
	// it should not stall the pipeline.
	pipe := w.d.RDB.Pipeline()
	for id, encoded := range latest {
		pipe.HSet(ctx, stream.FleetStateKey, id, encoded)
	}
	for id, points := range timeline {
		tk := stream.TimelineKey(id)
		pipe.RPush(ctx, tk, points...)
		pipe.LTrim(ctx, tk, -w.d.TimelineSamples, -1)
		pipe.Expire(ctx, tk, 2*time.Hour)
	}
	for _, m := range live {
		pipe.Publish(ctx, stream.VehicleChannel(m.id), m.encoded)
	}
	if processed > 0 {
		counter := throughputKey(time.Now())
		pipe.IncrBy(ctx, counter, processed)
		pipe.Expire(ctx, counter, 5*time.Minute)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		w.d.Metrics.errors.WithLabelValues("live_state").Inc()
		slog.Warn("live state write failed", "error", err)
	}

	if err := w.d.RDB.XAck(ctx, key, stream.Group, ids...).Err(); err != nil {
		// A redelivered entry is dropped by the sequence check, so only log it.
		w.d.Metrics.errors.WithLabelValues("ack").Inc()
	}
	w.d.Metrics.events.Add(float64(processed))
	return true
}

type liveMessage struct {
	id      string
	encoded []byte
}

func throughputKey(t time.Time) string { return "fleet:events:" + t.UTC().Format("200601021504") }

func (w *shardWorker) saveDurable(ctx context.Context, vehicles []store.VehicleMeta, diagnoses []diag.Diagnosis) error {
	backoff := 200 * time.Millisecond
	for {
		err := w.d.Store.UpsertVehicles(ctx, vehicles)
		if err == nil {
			err = w.d.Store.InsertDiagnostics(ctx, diagnoses)
			if err == nil {
				return nil
			}
			vehicles = nil
		}
		w.d.Metrics.errors.WithLabelValues("postgres").Inc()
		slog.Warn("durable write failed, retrying", "shard", w.shard, "error", err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sleep(ctx, backoff)
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (w *shardWorker) entry(ctx context.Context, id string) (*vehicleEntry, error) {
	if e, ok := w.vehicles[id]; ok {
		return e, nil
	}
	state := &diag.VehicleState{}
	raw, err := w.d.RDB.Get(ctx, stream.StateKey(id)).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, state); err != nil {
			// Start fresh rather than wedge the shard on a corrupt snapshot.
			slog.Warn("discarding corrupt vehicle state", "vehicle", id, "error", err)
			state = &diag.VehicleState{}
		}
	}
	e := &vehicleEntry{state: state}
	w.vehicles[id] = e
	w.d.Metrics.vehicles.Set(float64(w.d.tracked.Add(1)))
	return e, nil
}

// snapshot persists analyzer state so that another pod can take over this shard.
func (w *shardWorker) snapshot(ctx context.Context) {
	pipe := w.d.RDB.Pipeline()
	written := make([]*vehicleEntry, 0)
	for id, e := range w.vehicles {
		if !e.dirty {
			continue
		}
		raw, err := json.Marshal(e.state)
		if err != nil {
			continue
		}
		pipe.Set(ctx, stream.StateKey(id), raw, 7*24*time.Hour)
		written = append(written, e)
	}
	if len(written) == 0 {
		return
	}
	if _, err := pipe.Exec(ctx); err != nil {
		w.d.Metrics.errors.WithLabelValues("snapshot").Inc()
		slog.Warn("state snapshot failed", "shard", w.shard, "error", err)
		return
	}
	for _, e := range written {
		e.dirty = false
	}
}

func (w *shardWorker) addRollup(ev *telemetryv1.TelemetryEvent, f diag.Features) {
	bucket := time.UnixMilli(ev.TimestampMs).UTC().Truncate(time.Minute)
	key := ev.VehicleId + bucket.Format("|200601021504")
	r := w.rollups[key]
	if r == nil {
		r = &rollupAcc{Rollup: store.Rollup{VehicleID: ev.VehicleId, Bucket: bucket}}
		w.rollups[key] = r
	}
	r.Samples++
	r.BatteryTemp += ev.BatteryTempC
	r.MotorTemp += ev.MotorTempC
	r.Ambient += ev.AmbientTempC
	r.SoC += ev.SocPct
	r.Speed += ev.SpeedKph
	r.PackPower += ev.PackPowerKw
	if f.BatteryZ != 0 {
		r.residualSum += f.BatteryResidual
		r.residualN++
	}
}

// flushRollups writes completed minutes. Rollups support history, not detection, so a
// failed write is counted and dropped instead of blocking the telemetry path.
func (w *shardWorker) flushRollups(ctx context.Context, now time.Time) {
	cutoff := now.UTC().Truncate(time.Minute)
	var out []store.Rollup
	for key, r := range w.rollups {
		if !r.Bucket.Before(cutoff) {
			continue
		}
		n := float64(r.Samples)
		row := r.Rollup
		row.BatteryTemp /= n
		row.MotorTemp /= n
		row.Ambient /= n
		row.SoC /= n
		row.Speed /= n
		row.PackPower /= n
		if r.residualN > 0 {
			avg := r.residualSum / float64(r.residualN)
			row.Residual = &avg
		}
		out = append(out, row)
		delete(w.rollups, key)
	}
	if err := w.d.Store.InsertRollups(ctx, out); err != nil {
		w.d.Metrics.errors.WithLabelValues("rollups").Inc()
		slog.Warn("rollup write failed", "shard", w.shard, "rows", len(out), "error", err)
	}
}

func buildLive(ev *telemetryv1.TelemetryEvent, f diag.Features) telemetry.LiveState {
	return telemetry.LiveState{
		VehicleID: ev.VehicleId, TimestampMs: ev.TimestampMs, Model: ev.Model, Region: ev.Region,
		SoftwareVersion: ev.SoftwareVersion, SocPct: ev.SocPct, BatteryTempC: ev.BatteryTempC,
		MotorTempC: ev.MotorTempC, CoolantTempC: ev.CoolantTempC, AmbientTempC: ev.AmbientTempC,
		CoolingDemandPct: ev.CoolingDemandPct, SpeedKph: ev.SpeedKph, Latitude: ev.Latitude,
		Longitude: ev.Longitude, HeadingDeg: ev.HeadingDeg, OdometerKm: ev.OdometerKm,
		ChargingState: strings.TrimPrefix(ev.ChargingState.String(), "CHARGING_STATE_"),
		ChargePowerKw: ev.ChargePowerKw, PackPowerKw: ev.PackPowerKw,
		Connectivity: strings.TrimPrefix(ev.Connectivity.String(), "CONNECTIVITY_STATE_"),
		SignalDbm:    ev.SignalDbm, Mode: f.Mode, Health: f.Health, Deviation: f.Deviation,
		BatteryResidual: f.BatteryResidual, WhPerKm: f.WhPerKm, RiseRate: f.RiseRate,
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
