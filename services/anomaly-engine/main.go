// Command anomaly-engine consumes telemetry from Redis streams, explains abnormal vehicle
// behaviour and writes diagnostics to Postgres. Pods share work through shard leases, so
// the service scales horizontally and survives pod loss without losing data.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ajtang/Intelletry/internal/diag"
	"github.com/ajtang/Intelletry/internal/platform"
	"github.com/ajtang/Intelletry/internal/store"
	"github.com/ajtang/Intelletry/internal/stream"
)

func main() {
	log := platform.NewLogger("anomaly-engine")
	ctx, stop := platform.SignalContext()
	defer stop()

	shutdownTracing, err := platform.InitTracing(ctx, "anomaly-engine")
	if err != nil {
		log.Error("tracing init failed", "error", err)
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
	retention := platform.EnvInt("ROLLUP_RETENTION_DAYS", 14)
	if err := st.MaintainPartitions(ctx, time.Now(), retention); err != nil {
		log.Error("partition maintenance failed", "error", err)
		os.Exit(1)
	}

	hostname, _ := os.Hostname()
	consumer := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	reg := platform.NewRegistry()
	metrics := NewMetrics(reg)
	fleet := NewFleetStats(rdb, platform.EnvInt("FLEET_WINDOW_MINUTES", 10))
	deps := &Deps{
		RDB: rdb, Store: st, Fleet: fleet, Metrics: metrics, Consumer: consumer,
		Analyzer:        diag.NewAnalyzer(diag.Config{TimeScale: platform.EnvFloat("TELEMETRY_TIME_SCALE", 1)}),
		TimelineSamples: int64(platform.EnvInt("TIMELINE_SAMPLES", 180)),
		SnapshotEvery:   platform.EnvDuration("STATE_SNAPSHOT_INTERVAL", 15*time.Second),
	}

	coord := &Coordinator{
		rdb: rdb, id: consumer, shards: platform.EnvInt("STREAM_SHARDS", 16),
		leaseTTL: platform.EnvDuration("LEASE_TTL", 15*time.Second),
		tick:     platform.EnvDuration("COORDINATOR_TICK", 3*time.Second),
		owned:    map[int]*ownedShard{},
		start: func(ctx context.Context, shard int) (<-chan struct{}, func()) {
			worker := newShardWorker(shard, deps)
			done := make(chan struct{})
			go func() {
				defer close(done)
				worker.run(ctx)
			}()
			return done, func() { worker.fenced.Store(true) }
		},
	}

	hc := platform.NewHealth()
	hc.Add("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() })
	hc.Add("postgres", st.Ping)
	mux := http.NewServeMux()
	hc.Register(mux, reg)
	srv := &http.Server{Addr: ":" + platform.Env("HTTP_PORT", "8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		coord.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		runFleetJobs(ctx, fleet, coord, st, metrics, retention, log)
	}()
	if err := platform.Serve(ctx, srv, 0, log); err != nil {
		log.Error("http server failed", "error", err)
	}
	wg.Wait()
	flush, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = fleet.Flush(flush, time.Now())
	_ = shutdownTracing(flush)
	log.Info("shutdown complete")
}

// runFleetJobs handles fleet statistics on every pod and leader-only duties.
func runFleetJobs(ctx context.Context, fleet *FleetStats, coord *Coordinator, st *store.Store, m *Metrics, retention int, log *slog.Logger) {
	reg := newRegressionTracker()
	var loaded bool
	var lastStats, lastPartitions, lastHistory time.Time
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		if err := fleet.Flush(ctx, now); err != nil {
			m.errors.WithLabelValues("fleet_flush").Inc()
		}
		if err := fleet.Refresh(ctx, now); err != nil {
			m.errors.WithLabelValues("fleet_refresh").Inc()
			log.Warn("fleet refresh failed", "error", err)
			continue
		}
		m.owned.Set(float64(coord.OwnedCount()))
		if now.Sub(lastHistory) >= 5*time.Minute {
			lastHistory = now
			if h, err := st.HistoricalResiduals(ctx, now.AddDate(0, 0, -7), now.Add(-time.Hour), 30); err == nil {
				fleet.SetHistory(h)
			} else {
				m.errors.WithLabelValues("history").Inc()
			}
		}
		if !coord.IsLeader() {
			loaded = false
			continue
		}
		if !loaded {
			// A new leader inherits open findings, so they can still clear later.
			if err := reg.load(ctx, st); err != nil {
				m.errors.WithLabelValues("regression_load").Inc()
				continue
			}
			loaded = true
		}
		changes := reg.observe(fleet.Findings(), now)
		if err := st.InsertDiagnostics(ctx, changes); err != nil {
			reg.rollback(changes)
			m.errors.WithLabelValues("regression_save").Inc()
		} else {
			for _, d := range changes {
				m.diagnostics.WithLabelValues(string(d.Category), string(d.Transition)).Inc()
				log.Info("fleet diagnosis", "subject", d.Subject, "transition", d.Transition, "reason", d.Reason)
			}
		}
		if now.Sub(lastStats) >= time.Minute {
			lastStats = now
			if err := st.InsertFleetStats(ctx, now.UTC().Truncate(time.Minute), fleetLevelStats(fleet.Current().Stats)); err != nil {
				m.errors.WithLabelValues("fleet_stats").Inc()
			}
		}
		if now.Sub(lastPartitions) >= time.Hour {
			lastPartitions = now
			if err := st.MaintainPartitions(ctx, now, retention); err != nil {
				m.errors.WithLabelValues("partitions").Inc()
			}
		}
	}
}

const (
	regressionConfirm = 3
	regressionClear   = 24 // about two minutes at the job interval
)

// regressionTracker adds hysteresis to fleet-level software findings, which are
// expensive to raise wrongly: they can pause a rollout.
type regressionTracker struct {
	seen   map[string]int
	missed map[string]int
	active map[string]bool
}

func newRegressionTracker() *regressionTracker {
	return &regressionTracker{seen: map[string]int{}, missed: map[string]int{}, active: map[string]bool{}}
}

func (r *regressionTracker) load(ctx context.Context, st *store.Store) error {
	rows, err := st.Pool.Query(ctx, `SELECT subject FROM alerts WHERE category = $1 AND status <> 'resolved'`, diag.CategorySoftwareRegression)
	if err != nil {
		return err
	}
	subjects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, s := range subjects {
		r.active[s[len("software:"):]] = true
	}
	return nil
}

func (r *regressionTracker) observe(findings []diag.VersionFinding, now time.Time) []diag.Diagnosis {
	byVersion := map[string][]diag.VersionFinding{}
	for _, f := range findings {
		byVersion[f.Version] = append(byVersion[f.Version], f)
	}
	var out []diag.Diagnosis
	for version, fs := range byVersion {
		r.missed[version] = 0
		r.seen[version]++
		if !r.active[version] && r.seen[version] >= regressionConfirm {
			r.active[version] = true
			d := diag.RegressionDiagnosis(version, fs)
			d.Transition, d.ObservedAt = diag.TransitionOpened, now
			out = append(out, d)
		}
	}
	for version := range r.active {
		if _, ok := byVersion[version]; ok {
			continue
		}
		r.seen[version] = 0
		r.missed[version]++
		if r.missed[version] >= regressionClear {
			delete(r.active, version)
			out = append(out, diag.Diagnosis{
				Subject: "software:" + version, Category: diag.CategorySoftwareRegression, Transition: diag.TransitionCleared,
				Severity: diag.SeverityWarning, Reason: "Condition no longer observed.", LikelyCause: "",
				Evidence: []diag.Evidence{}, Actions: []string{}, SoftwareVersion: version, ObservedAt: now,
			})
		}
	}
	return out
}

// rollback undoes observe after a failed save, so the change is retried on the next run.
func (r *regressionTracker) rollback(changes []diag.Diagnosis) {
	for _, d := range changes {
		version := d.SoftwareVersion
		if d.Transition == diag.TransitionOpened {
			delete(r.active, version)
		} else {
			r.active[version] = true
		}
	}
}
