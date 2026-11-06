// Package store holds the Postgres access shared by the engine and the alert service.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ajtang/Intelletry/internal/diag"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationLock = 727001

// Store wraps a connection pool.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres.
func Open(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnLifetime = 30 * time.Minute // lets connections rebalance after a failover
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Ping checks connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

// Migrate applies pending migrations. An advisory lock lets every replica call this
// at startup without a separate migration job or a race between pods.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return err
	}
	defer func() {
		// The lock is session scoped, so release it even if ctx has been cancelled.
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(release, `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name=$1)`, e.Name()).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, e.Name()); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// MaintainPartitions creates upcoming rollup partitions and drops expired ones.
func (s *Store) MaintainPartitions(ctx context.Context, now time.Time, retentionDays int) error {
	day := now.UTC().Truncate(24 * time.Hour)
	for i := -1; i <= 3; i++ {
		from := day.AddDate(0, 0, i)
		name := "telemetry_rollups_" + from.Format("20060102")
		sql := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF telemetry_rollups FOR VALUES FROM ('%s') TO ('%s')`,
			pgx.Identifier{name}.Sanitize(), from.Format(time.RFC3339), from.AddDate(0, 0, 1).Format(time.RFC3339))
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("create partition %s: %w", name, err)
		}
	}
	rows, err := s.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'telemetry_rollups'`)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	cutoff := day.AddDate(0, 0, -retentionDays).Format("20060102")
	for _, name := range names {
		if len(name) >= 8 && name[len(name)-8:] < cutoff {
			if _, err := s.Pool.Exec(ctx, `DROP TABLE IF EXISTS `+pgx.Identifier{name}.Sanitize()); err != nil {
				return err
			}
		}
	}
	return nil
}

// VehicleMeta is registry data.
type VehicleMeta struct {
	VehicleID, VIN, Model, Region, SoftwareVersion string
	PackCapacityKWh                                float64
}

// UpsertVehicles registers vehicles, updating only rows whose data changed.
func (s *Store) UpsertVehicles(ctx context.Context, vs []VehicleMeta) error {
	if len(vs) == 0 {
		return nil
	}
	n := len(vs)
	ids, vins, models, regions, sw := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	caps := make([]float64, n)
	for i, v := range vs {
		ids[i], vins[i], models[i], regions[i], sw[i], caps[i] = v.VehicleID, v.VIN, v.Model, v.Region, v.SoftwareVersion, v.PackCapacityKWh
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO vehicles (vehicle_id, vin, model, region, software_version, pack_capacity_kwh)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::float8[])
		ON CONFLICT (vehicle_id) DO UPDATE SET software_version = EXCLUDED.software_version, updated_at = now()
		WHERE vehicles.software_version <> EXCLUDED.software_version`, ids, vins, models, regions, sw, caps)
	return err
}

// InsertDiagnostics writes diagnoses atomically into the outbox.
func (s *Store) InsertDiagnostics(ctx context.Context, ds []diag.Diagnosis) error {
	if len(ds) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, d := range ds {
			ev, err := json.Marshal(d.Evidence)
			if err != nil {
				return err
			}
			actions, err := json.Marshal(d.Actions)
			if err != nil {
				return err
			}
			b.Queue(`INSERT INTO diagnostics (subject, category, transition, severity, confidence, deviation_score,
				reason, likely_cause, evidence, recommended_actions, software_version, observed_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
				d.Subject, d.Category, d.Transition, d.Severity, d.Confidence, d.DeviationScore,
				d.Reason, d.LikelyCause, ev, actions, d.SoftwareVersion, d.ObservedAt)
		}
		return tx.SendBatch(ctx, b).Close()
	})
}

// Rollup is one aggregated minute of vehicle telemetry.
type Rollup struct {
	VehicleID                                              string
	Bucket                                                 time.Time
	Samples                                                int32
	BatteryTemp, MotorTemp, Ambient, SoC, Speed, PackPower float64
	Residual                                               *float64
}

// InsertRollups writes rollups idempotently, because replay after a crash may
// produce the same bucket again.
func (s *Store) InsertRollups(ctx context.Context, rs []Rollup) error {
	if len(rs) == 0 {
		return nil
	}
	n := len(rs)
	ids, buckets := make([]string, n), make([]time.Time, n)
	samples := make([]int32, n)
	batt, motor, amb, soc, speed, power := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	resid := make([]*float64, n)
	for i, r := range rs {
		ids[i], buckets[i], samples[i] = r.VehicleID, r.Bucket, r.Samples
		batt[i], motor[i], amb[i], soc[i], speed[i], power[i], resid[i] = r.BatteryTemp, r.MotorTemp, r.Ambient, r.SoC, r.Speed, r.PackPower, r.Residual
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO telemetry_rollups (vehicle_id, bucket, samples, battery_temp_c, motor_temp_c, ambient_temp_c,
			soc_pct, speed_kph, pack_power_kw, battery_residual)
		SELECT * FROM unnest($1::text[], $2::timestamptz[], $3::int4[], $4::float8[], $5::float8[], $6::float8[],
			$7::float8[], $8::float8[], $9::float8[], $10::float8[])
		ON CONFLICT (vehicle_id, bucket) DO NOTHING`,
		ids, buckets, samples, batt, motor, amb, soc, speed, power, resid)
	return err
}

// HistoricalResiduals returns each vehicle's long-term battery temperature residual,
// excluding the recent past so that a developing fault does not raise its own baseline.
func (s *Store) HistoricalResiduals(ctx context.Context, from, to time.Time, minSamples int) (map[string]float64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT vehicle_id, avg(battery_residual) FROM telemetry_rollups
		WHERE bucket >= $1 AND bucket < $2 AND battery_residual IS NOT NULL
		GROUP BY vehicle_id HAVING count(*) >= $3`, from, to, minSamples)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var id string
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// InsertFleetStats records a snapshot of cohort statistics.
func (s *Store) InsertFleetStats(ctx context.Context, at time.Time, stats map[string]diag.Stat) error {
	if len(stats) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for key, st := range stats {
		metric, cohort := key, ""
		for i := 0; i < len(key); i++ {
			if key[i] == '|' {
				metric, cohort = key[:i], key[i+1:]
				break
			}
		}
		b.Queue(`INSERT INTO fleet_statistics (captured_at, metric, cohort, median, mad, samples)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, at, metric, cohort, st.Median, st.MAD, st.N)
	}
	return s.Pool.SendBatch(ctx, b).Close()
}
