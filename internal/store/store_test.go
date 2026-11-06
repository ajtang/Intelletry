package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ajtang/Intelletry/internal/diag"
)

// openTest connects to a disposable database named by TEST_DATABASE_URL. The test
// recreates the public schema, so it must never point at a database you care about.
func openTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreIntegration(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Replicas all migrate at startup, so concurrent runs must be safe.
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errs <- s.Migrate(ctx) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	if err := s.MaintainPartitions(ctx, now, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainPartitions(ctx, now, 7); err != nil {
		t.Fatalf("partition maintenance must be repeatable: %v", err)
	}

	// Rollups replay after a crash, so duplicates must be ignored.
	residual := 1.5
	rollups := []Rollup{
		{VehicleID: "EV-1", Bucket: now, Samples: 3, BatteryTemp: 31, Residual: &residual},
		{VehicleID: "EV-2", Bucket: now, Samples: 2, BatteryTemp: 28},
	}
	for i := 0; i < 2; i++ {
		if err := s.InsertRollups(ctx, rollups); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM telemetry_rollups`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rollups = %d, want 2", count)
	}
	hist, err := s.HistoricalResiduals(ctx, now.Add(-time.Hour), now.Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	if hist["EV-1"] != 1.5 || len(hist) != 1 {
		t.Fatalf("history = %v", hist)
	}

	// Old partitions are dropped; current ones survive.
	if err := s.MaintainPartitions(ctx, now.AddDate(0, 0, 30), 7); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT to_regclass('telemetry_rollups_20260610') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expired partition should have been dropped")
	}

	// Vehicle upserts track software rollouts.
	v := VehicleMeta{VehicleID: "EV-1", VIN: "V", Model: "M", Region: "R", SoftwareVersion: "1", PackCapacityKWh: 80}
	if err := s.UpsertVehicles(ctx, []VehicleMeta{v}); err != nil {
		t.Fatal(err)
	}
	v.SoftwareVersion = "2"
	if err := s.UpsertVehicles(ctx, []VehicleMeta{v}); err != nil {
		t.Fatal(err)
	}
	var sw string
	if err := s.Pool.QueryRow(ctx, `SELECT software_version FROM vehicles WHERE vehicle_id='EV-1'`).Scan(&sw); err != nil {
		t.Fatal(err)
	}
	if sw != "2" {
		t.Fatalf("software_version = %q", sw)
	}

	// Diagnostics enter the outbox unprocessed, with evidence intact.
	err = s.InsertDiagnostics(ctx, []diag.Diagnosis{{
		Subject: "EV-1", Category: diag.CategoryCoolingFailure, Transition: diag.TransitionOpened,
		Severity: diag.SeverityWarning, Confidence: 0.91, Reason: "r", LikelyCause: "c",
		Evidence: []diag.Evidence{{Signal: "s", Detail: "d", Weight: 2}}, Actions: []string{"a"}, ObservedAt: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var signal string
	if err := s.Pool.QueryRow(ctx, `SELECT evidence->0->>'signal' FROM diagnostics WHERE processed_at IS NULL`).Scan(&signal); err != nil {
		t.Fatal(err)
	}
	if signal != "s" {
		t.Fatalf("evidence signal = %q", signal)
	}

	// Fleet statistics split metric and cohort at the first separator.
	err = s.InsertFleetStats(ctx, now, map[string]diag.Stat{"battery_temp|dc|a20": {Median: 30, MAD: 2, N: 100}})
	if err != nil {
		t.Fatal(err)
	}
	var cohort string
	if err := s.Pool.QueryRow(ctx, `SELECT cohort FROM fleet_statistics WHERE metric='battery_temp'`).Scan(&cohort); err != nil {
		t.Fatal(err)
	}
	if cohort != "dc|a20" {
		t.Fatalf("cohort = %q", cohort)
	}
}
