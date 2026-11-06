package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ajtang/Intelletry/internal/diag"
	"github.com/ajtang/Intelletry/internal/store"
	"github.com/ajtang/Intelletry/internal/stream"
	"github.com/ajtang/Intelletry/internal/telemetry"
)

const testToken = "test-token-0123456789"

type env struct {
	st   *store.Store
	proc *Processor
	srv  *httptest.Server
}

func setup(t *testing.T) *env {
	t.Helper()
	dsn, redisURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("TEST_DATABASE_URL and TEST_REDIS_URL not set")
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
	rdb, err := stream.NewClient(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	live, _ := json.Marshal(telemetry.LiveState{VehicleID: "EV-1", TimestampMs: time.Now().UnixMilli(), Health: "warning", Mode: "dc", SocPct: 40, BatteryTempC: 47, SoftwareVersion: "1"})
	rdb.HSet(ctx, stream.FleetStateKey, "EV-1", live)
	stale, _ := json.Marshal(telemetry.LiveState{VehicleID: "EV-2", TimestampMs: time.Now().Add(-time.Hour).UnixMilli(), Health: "healthy", Mode: "idle", SocPct: 80, SoftwareVersion: "1"})
	rdb.HSet(ctx, stream.FleetStateKey, "EV-2", stale)
	if err := st.UpsertVehicles(ctx, []store.VehicleMeta{{VehicleID: "EV-1", VIN: "V", Model: "M", Region: "R", SoftwareVersion: "1", PackCapacityKWh: 80}}); err != nil {
		t.Fatal(err)
	}
	api := &API{st: st, rdb: rdb, token: testToken, hub: NewHub(nil), offlineAfter: 30 * time.Second, stopStreams: make(chan struct{})}
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{st, &Processor{st: st, rdb: rdb}, srv}
}

func (e *env) diagnose(t *testing.T, transition diag.Transition, sev diag.Severity) {
	t.Helper()
	ctx := context.Background()
	err := e.st.InsertDiagnostics(ctx, []diag.Diagnosis{{
		Subject: "EV-1", Category: diag.CategoryCoolingFailure, Transition: transition, Severity: sev, Confidence: 0.9,
		Reason: "hot", LikelyCause: "pump", Evidence: []diag.Evidence{{Signal: "s", Detail: "d", Weight: 1}},
		Actions: []string{"check coolant"}, ObservedAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.proc.Step(ctx); err != nil {
		t.Fatal(err)
	}
}

func (e *env) do(t *testing.T, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (e *env) alerts(t *testing.T, query string) []Alert {
	t.Helper()
	var out struct {
		Alerts []Alert `json:"alerts"`
	}
	if code := e.do(t, "GET", "/api/v1/alerts?"+query, "", &out); code != 200 {
		t.Fatalf("list alerts: %d", code)
	}
	return out.Alerts
}

func TestAlertLifecycle(t *testing.T) {
	e := setup(t)

	e.diagnose(t, diag.TransitionOpened, diag.SeverityWarning)
	open := e.alerts(t, "status=open")
	if len(open) != 1 || open[0].Status != "active" || open[0].Level != "warning" || open[0].Title != "Potential Battery Cooling Failure" {
		t.Fatalf("after open: %+v", open)
	}
	id := itoa(open[0].ID)

	// Repeated detections update one alert rather than creating noise.
	e.diagnose(t, diag.TransitionUpdated, diag.SeverityWarning)
	open = e.alerts(t, "status=open")
	if len(open) != 1 || open[0].Occurrences != 2 {
		t.Fatalf("after update: %+v", open)
	}

	if code := e.do(t, "POST", "/api/v1/alerts/"+id+"/acknowledge", `{}`, nil); code != 400 {
		t.Fatalf("ack without actor: %d", code)
	}
	if code := e.do(t, "POST", "/api/v1/alerts/"+id+"/acknowledge", `{"actor":"sam"}`, nil); code != 200 {
		t.Fatalf("ack: %d", code)
	}
	if code := e.do(t, "POST", "/api/v1/alerts/"+id+"/acknowledge", `{"actor":"sam"}`, nil); code != 409 {
		t.Fatalf("double ack: %d", code)
	}

	// Escalation must demand fresh attention.
	e.diagnose(t, diag.TransitionUpdated, diag.SeverityCritical)
	open = e.alerts(t, "status=open")
	if open[0].Level != "critical" || open[0].Status != "active" {
		t.Fatalf("after escalation: %+v", open[0])
	}

	e.diagnose(t, diag.TransitionCleared, diag.SeverityCritical)
	if open = e.alerts(t, "status=open"); len(open) != 0 {
		t.Fatalf("clear should resolve: %+v", open)
	}

	var detail struct {
		History []struct {
			EventType string `json:"event_type"`
		} `json:"history"`
	}
	e.do(t, "GET", "/api/v1/alerts/"+id, "", &detail)
	var kinds []string
	for _, h := range detail.History {
		kinds = append(kinds, h.EventType)
	}
	if got, want := strings.Join(kinds, ","), "created,updated,acknowledged,escalated,auto_resolved"; got != want {
		t.Fatalf("history = %s, want %s", got, want)
	}

	// A recurrence opens a new alert, keeping the resolved one as history.
	e.diagnose(t, diag.TransitionOpened, diag.SeverityWarning)
	if open = e.alerts(t, "status=open"); len(open) != 1 || itoa(open[0].ID) == id {
		t.Fatalf("recurrence: %+v", open)
	}
	if code := e.do(t, "POST", "/api/v1/alerts/"+itoa(open[0].ID)+"/resolve", `{"actor":"sam","note":"fixed"}`, nil); code != 200 {
		t.Fatalf("resolve: %d", code)
	}
	if code := e.do(t, "POST", "/api/v1/alerts/"+itoa(open[0].ID)+"/resolve", `{"actor":"sam"}`, nil); code != 409 {
		t.Fatalf("double resolve: %d", code)
	}
	if code := e.do(t, "POST", "/api/v1/alerts/999999/resolve", `{"actor":"sam"}`, nil); code != 404 {
		t.Fatalf("missing alert: %d", code)
	}
	// A late "cleared" for something an operator resolved must not fail the outbox.
	e.diagnose(t, diag.TransitionCleared, diag.SeverityWarning)
	var pending int
	if err := e.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM diagnostics WHERE processed_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("outbox not drained: %d %v", pending, err)
	}
}

func TestOnlyOneProcessorAtATime(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.st.InsertDiagnostics(ctx, []diag.Diagnosis{{
		Subject: "EV-1", Category: diag.CategorySensorDrift, Transition: diag.TransitionOpened, Severity: diag.SeverityInfo,
		Evidence: []diag.Evidence{}, Actions: []string{}, ObservedAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	tx, err := e.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, processorLock); err != nil {
		t.Fatal(err)
	}
	if n, err := e.proc.Step(ctx); err != nil || n != 0 {
		t.Fatalf("second processor should stand down: n=%d err=%v", n, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := e.proc.Step(ctx); err != nil || n != 1 {
		t.Fatalf("processor should resume: n=%d err=%v", n, err)
	}
}

func TestReadEndpoints(t *testing.T) {
	e := setup(t)
	e.diagnose(t, diag.TransitionOpened, diag.SeverityWarning)

	var ov struct {
		Total        int `json:"total_vehicles"`
		Online       int `json:"online"`
		Offline      int `json:"offline"`
		Warning      int `json:"warning"`
		ActiveAlerts int `json:"active_alerts"`
	}
	if code := e.do(t, "GET", "/api/v1/fleet/overview", "", &ov); code != 200 {
		t.Fatalf("overview: %d", code)
	}
	if ov.Total != 2 || ov.Online != 1 || ov.Offline != 1 || ov.Warning != 1 || ov.ActiveAlerts != 1 {
		t.Fatalf("overview = %+v", ov)
	}
	for _, path := range []string{
		"/api/v1/fleet/positions", "/api/v1/vehicles", "/api/v1/vehicles?health=warning&q=ev-1", "/api/v1/vehicles/EV-1",
		"/api/v1/vehicles/EV-1/timeline", "/api/v1/vehicles/EV-1/timeline?minutes=120", "/api/v1/diagnostics",
		"/api/v1/alerts/trends?hours=24", "/api/v1/alerts?level=warning&subject=EV-1",
	} {
		var out any
		if code := e.do(t, "GET", path, "", &out); code != 200 {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	for path, want := range map[string]int{"/api/v1/vehicles/NOPE": 404, "/api/v1/alerts?status=bogus": 400, "/api/v1/vehicles?limit=0": 400} {
		if code := e.do(t, "GET", path, "", nil); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	resp, err := http.Get(e.srv.URL + "/api/v1/alerts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
}

func itoa(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
