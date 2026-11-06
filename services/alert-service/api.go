package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"

	"github.com/ajtang/Intelletry/internal/store"
	"github.com/ajtang/Intelletry/internal/stream"
	"github.com/ajtang/Intelletry/internal/telemetry"
)

// API is the operations read and action surface used by the fleet dashboard.
type API struct {
	st           *store.Store
	rdb          *redis.Client
	token        string
	offlineAfter time.Duration
	hub          *Hub
	duration     *prometheus.HistogramVec
	online       prometheus.Gauge
	total        prometheus.Gauge
	stopStreams  chan struct{}

	cacheMu sync.Mutex
	cached  *fleetSnapshot
}

type vehicleView struct {
	telemetry.LiveState
	Online bool `json:"online"`
}

type fleetSnapshot struct {
	at       time.Time
	vehicles []vehicleView
}

// fleet returns live state, cached briefly. Many dashboards polling a 10k-field HGETALL
// would otherwise hurt the Redis instance that also carries the telemetry stream. The
// mutex also collapses concurrent misses into one read.
func (a *API) fleet(ctx context.Context) (*fleetSnapshot, error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	now := time.Now()
	if a.cached != nil && now.Sub(a.cached.at) < 2*time.Second {
		return a.cached, nil
	}
	raw, err := a.rdb.HGetAll(ctx, stream.FleetStateKey).Result()
	if err != nil {
		return nil, err
	}
	vehicles := make([]vehicleView, 0, len(raw))
	online := 0
	for _, v := range raw {
		var s telemetry.LiveState
		if json.Unmarshal([]byte(v), &s) != nil {
			continue
		}
		view := vehicleView{LiveState: s, Online: now.Sub(time.UnixMilli(s.TimestampMs)) < a.offlineAfter}
		if view.Online {
			online++
		}
		vehicles = append(vehicles, view)
	}
	sort.Slice(vehicles, func(i, j int) bool { return vehicles[i].VehicleID < vehicles[j].VehicleID })
	a.cached = &fleetSnapshot{at: now, vehicles: vehicles}
	if a.online != nil {
		a.online.Set(float64(online))
		a.total.Set(float64(len(vehicles)))
	}
	return a.cached, nil
}

// Routes builds the HTTP handler.
func (a *API) Routes(mux *http.ServeMux) {
	route := func(pattern string, h func(http.ResponseWriter, *http.Request) error) {
		mux.Handle(pattern, a.middleware(pattern, h))
	}
	route("GET /api/v1/fleet/overview", a.overview)
	route("GET /api/v1/fleet/positions", a.positions)
	route("GET /api/v1/vehicles", a.listVehicles)
	route("GET /api/v1/vehicles/{id}", a.vehicle)
	route("GET /api/v1/vehicles/{id}/timeline", a.timeline)
	route("GET /api/v1/vehicles/{id}/stream", a.vehicleStream)
	route("GET /api/v1/alerts", a.listAlerts)
	route("GET /api/v1/alerts/trends", a.trends)
	route("GET /api/v1/alerts/{id}", a.alertDetail)
	route("POST /api/v1/alerts/{id}/acknowledge", a.alertAction("acknowledged"))
	route("POST /api/v1/alerts/{id}/resolve", a.alertAction("resolved"))
	route("GET /api/v1/diagnostics", a.listDiagnostics)
	route("GET /api/v1/events", a.events)
}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// statusRecorder captures the response code while keeping streaming support.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *API) middleware(pattern string, h func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := otel.Tracer("alert-service").Start(r.Context(), pattern)
		defer span.End()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var err error
		if subtle.ConstantTimeCompare([]byte(given), []byte(a.token)) != 1 {
			err = &apiError{http.StatusUnauthorized, "invalid or missing token"}
		} else {
			err = h(rec, r.WithContext(ctx))
		}
		if err != nil {
			var ae *apiError
			switch {
			case errors.As(err, &ae):
				writeJSON(rec, ae.code, map[string]string{"error": ae.msg})
			case errors.Is(err, pgx.ErrNoRows):
				writeJSON(rec, http.StatusNotFound, map[string]string{"error": "not found"})
			case errors.Is(err, context.Canceled):
			default:
				slog.Error("request failed", "route", pattern, "error", err)
				writeJSON(rec, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
		}
		if a.duration != nil {
			a.duration.WithLabelValues(pattern, strconv.Itoa(rec.code)).Observe(time.Since(start).Seconds())
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("response write failed", "error", err)
	}
}

func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, badRequest("%s must be an integer in [%d, %d]", name, lo, hi)
	}
	return n, nil
}

func oneOf(r *http.Request, name string, allowed ...string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", nil
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", badRequest("%s must be one of %s", name, strings.Join(allowed, ", "))
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) error {
	snap, err := a.fleet(r.Context())
	if err != nil {
		return err
	}
	type bucket struct {
		Version  string `json:"version"`
		Vehicles int    `json:"vehicles"`
	}
	out := struct {
		Total          int      `json:"total_vehicles"`
		Online         int      `json:"online"`
		Offline        int      `json:"offline"`
		Healthy        int      `json:"healthy"`
		Warning        int      `json:"warning"`
		Critical       int      `json:"critical"`
		Driving        int      `json:"driving"`
		Charging       int      `json:"charging"`
		Parked         int      `json:"parked"`
		AvgSoC         float64  `json:"avg_soc_pct"`
		AvgBatteryTemp float64  `json:"avg_battery_temp_c"`
		MaxBatteryTemp float64  `json:"max_battery_temp_c"`
		ActiveAlerts   int      `json:"active_alerts"`
		AlertsCritical int      `json:"alerts_critical"`
		AlertsWarning  int      `json:"alerts_warning"`
		AlertsInfo     int      `json:"alerts_info"`
		Unacknowledged int      `json:"alerts_unacknowledged"`
		EventsPerSec   float64  `json:"events_per_sec"`
		QueueDepth     int64    `json:"queue_depth"`
		Software       []bucket `json:"software_versions"`
	}{Total: len(snap.vehicles)}
	versions := map[string]int{}
	var soc, batt float64
	for _, v := range snap.vehicles {
		if !v.Online {
			out.Offline++
			continue
		}
		out.Online++
		soc += v.SocPct
		batt += v.BatteryTempC
		out.MaxBatteryTemp = math.Max(out.MaxBatteryTemp, v.BatteryTempC)
		versions[v.SoftwareVersion]++
		switch v.Health {
		case "critical":
			out.Critical++
		case "warning":
			out.Warning++
		default:
			out.Healthy++
		}
		switch {
		case v.Mode == "dc" || v.Mode == "ac":
			out.Charging++
		case v.Mode == "idle":
			out.Parked++
		default:
			out.Driving++
		}
	}
	if out.Online > 0 {
		out.AvgSoC = soc / float64(out.Online)
		out.AvgBatteryTemp = batt / float64(out.Online)
	}
	for version, n := range versions {
		out.Software = append(out.Software, bucket{version, n})
	}
	sort.Slice(out.Software, func(i, j int) bool { return out.Software[i].Vehicles > out.Software[j].Vehicles })

	err = a.st.Pool.QueryRow(r.Context(), `SELECT count(*),
		count(*) FILTER (WHERE level='critical'), count(*) FILTER (WHERE level='warning'), count(*) FILTER (WHERE level='info'),
		count(*) FILTER (WHERE status='active') FROM alerts WHERE status <> 'resolved'`).
		Scan(&out.ActiveAlerts, &out.AlertsCritical, &out.AlertsWarning, &out.AlertsInfo, &out.Unacknowledged)
	if err != nil {
		return err
	}
	// The previous full minute, because the current one is still accumulating.
	if n, err := a.rdb.Get(r.Context(), "fleet:events:"+time.Now().Add(-time.Minute).UTC().Format("200601021504")).Int64(); err == nil {
		out.EventsPerSec = float64(n) / 60
	}
	pipe := a.rdb.Pipeline()
	for s := 0; s < streamShards; s++ {
		pipe.XInfoGroups(r.Context(), stream.Key(s))
	}
	if cmds, err := pipe.Exec(r.Context()); err == nil || errors.Is(err, redis.Nil) {
		for _, c := range cmds {
			if g, ok := c.(*redis.XInfoGroupsCmd); ok && g.Err() == nil {
				for _, info := range g.Val() {
					if info.Name == stream.Group {
						out.QueueDepth += info.Lag + info.Pending
					}
				}
			}
		}
	}
	writeJSON(w, 200, out)
	return nil
}

var streamShards = 16

func (a *API) positions(w http.ResponseWriter, r *http.Request) error {
	snap, err := a.fleet(r.Context())
	if err != nil {
		return err
	}
	type position struct {
		ID      string  `json:"id"`
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
		Heading float64 `json:"heading"`
		SoC     float64 `json:"soc"`
		Battery float64 `json:"battery_temp_c"`
		Speed   float64 `json:"speed_kph"`
		Mode    string  `json:"mode"`
		Health  string  `json:"health"`
		Online  bool    `json:"online"`
		Region  string  `json:"region"`
	}
	out := make([]position, len(snap.vehicles))
	for i, v := range snap.vehicles {
		out[i] = position{v.VehicleID, v.Latitude, v.Longitude, v.HeadingDeg, v.SocPct, v.BatteryTempC, v.SpeedKph, v.Mode, v.Health, v.Online, v.Region}
	}
	writeJSON(w, 200, out)
	return nil
}

func healthRank(v vehicleView) int {
	switch v.Health {
	case "critical":
		return 2
	case "warning":
		return 1
	}
	return 0
}

func (a *API) listVehicles(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if err != nil {
		return err
	}
	health, err := oneOf(r, "health", "healthy", "warning", "critical")
	if err != nil {
		return err
	}
	snap, err := a.fleet(r.Context())
	if err != nil {
		return err
	}
	q := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("q")))
	matched := make([]vehicleView, 0, limit)
	for _, v := range snap.vehicles {
		if health != "" && v.Health != health {
			continue
		}
		if q != "" && !strings.Contains(strings.ToUpper(v.VehicleID), q) {
			continue
		}
		matched = append(matched, v)
	}
	// The vehicles that need attention come first, since this is a triage view.
	sort.SliceStable(matched, func(i, j int) bool {
		if ri, rj := healthRank(matched[i]), healthRank(matched[j]); ri != rj {
			return ri > rj
		}
		return matched[i].Deviation > matched[j].Deviation
	})
	total := len(matched)
	if len(matched) > limit {
		matched = matched[:limit]
	}
	writeJSON(w, 200, map[string]any{"total": total, "vehicles": matched})
	return nil
}

func (a *API) vehicle(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	ctx := r.Context()
	meta := struct {
		VehicleID       string    `json:"vehicle_id"`
		VIN             string    `json:"vin"`
		Model           string    `json:"model"`
		Region          string    `json:"region"`
		PackCapacityKWh float64   `json:"pack_capacity_kwh"`
		SoftwareVersion string    `json:"software_version"`
		FirstSeenAt     time.Time `json:"first_seen_at"`
	}{}
	if err := a.st.Pool.QueryRow(ctx, `SELECT vehicle_id, vin, model, region, pack_capacity_kwh, software_version, first_seen_at
		FROM vehicles WHERE vehicle_id=$1`, id).Scan(&meta.VehicleID, &meta.VIN, &meta.Model, &meta.Region, &meta.PackCapacityKWh, &meta.SoftwareVersion, &meta.FirstSeenAt); err != nil {
		return err
	}
	var live *vehicleView
	raw, err := a.rdb.HGet(ctx, stream.FleetStateKey, id).Bytes()
	if err == nil {
		var s telemetry.LiveState
		if json.Unmarshal(raw, &s) == nil {
			live = &vehicleView{s, time.Since(time.UnixMilli(s.TimestampMs)) < a.offlineAfter}
		}
	} else if !errors.Is(err, redis.Nil) {
		return err
	}
	rows, err := a.st.Pool.Query(ctx, `SELECT `+alertColumns+` FROM alerts WHERE subject=$1 AND status <> 'resolved'
		ORDER BY level_rank(level) DESC, created_at DESC`, id)
	if err != nil {
		return err
	}
	alerts, err := pgx.CollectRows(rows, scanAlert)
	if err != nil {
		return err
	}
	rows, err = a.st.Pool.Query(ctx, `SELECT `+diagnosticColumns+` FROM diagnostics WHERE subject=$1 ORDER BY id DESC LIMIT 20`, id)
	if err != nil {
		return err
	}
	diagnostics, err := pgx.CollectRows(rows, scanDiagnostic)
	if err != nil {
		return err
	}
	// Recommended actions come from open problems only, most severe first and de-duplicated.
	seen := map[string]bool{}
	actions := []string{}
	for _, al := range alerts {
		for _, act := range al.Actions {
			if !seen[act] {
				seen[act] = true
				actions = append(actions, act)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"vehicle": meta, "live": live, "active_alerts": alerts, "diagnostics": diagnostics, "recommended_actions": actions})
	return nil
}

func (a *API) timeline(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	minutes, err := intParam(r, "minutes", 10, 1, 7*24*60)
	if err != nil {
		return err
	}
	points := []telemetry.TimelineSample{}
	source := "raw"
	if minutes <= 15 {
		raw, err := a.rdb.LRange(r.Context(), stream.TimelineKey(id), 0, -1).Result()
		if err != nil {
			return err
		}
		cutoff := time.Now().Add(-time.Duration(minutes) * time.Minute).UnixMilli()
		for _, item := range raw {
			var p telemetry.TimelineSample
			if json.Unmarshal([]byte(item), &p) == nil && p.T >= cutoff {
				points = append(points, p)
			}
		}
	} else {
		source = "rollup"
		rows, err := a.st.Pool.Query(r.Context(), `SELECT bucket, battery_temp_c, motor_temp_c, ambient_temp_c, soc_pct, speed_kph, pack_power_kw
			FROM telemetry_rollups WHERE vehicle_id=$1 AND bucket >= now() - make_interval(mins => $2) ORDER BY bucket`, id, minutes)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p telemetry.TimelineSample
			var t time.Time
			if err := rows.Scan(&t, &p.Battery, &p.Motor, &p.Ambient, &p.SoC, &p.Speed, &p.Power); err != nil {
				return err
			}
			p.T = t.UnixMilli()
			points = append(points, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	writeJSON(w, 200, map[string]any{"source": source, "points": points})
	return nil
}

func (a *API) listAlerts(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 100, 1, 500)
	if err != nil {
		return err
	}
	status, err := oneOf(r, "status", "open", "active", "acknowledged", "resolved")
	if err != nil {
		return err
	}
	level, err := oneOf(r, "level", "info", "warning", "critical")
	if err != nil {
		return err
	}
	before := int64(math.MaxInt64)
	if c := r.URL.Query().Get("before"); c != "" {
		if before, err = strconv.ParseInt(c, 10, 64); err != nil {
			return badRequest("before must be an alert id")
		}
	}
	rows, err := a.st.Pool.Query(r.Context(), `SELECT `+alertColumns+` FROM alerts WHERE id < $1
		AND ($2 = '' OR ($2 = 'open' AND status <> 'resolved') OR status = $2)
		AND ($3 = '' OR level = $3) AND ($4 = '' OR subject = $4)
		ORDER BY id DESC LIMIT $5`, before, status, level, r.URL.Query().Get("subject"), limit)
	if err != nil {
		return err
	}
	alerts, err := pgx.CollectRows(rows, scanAlert)
	if err != nil {
		return err
	}
	if alerts == nil {
		alerts = []Alert{}
	}
	writeJSON(w, 200, map[string]any{"alerts": alerts})
	return nil
}

func alertID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, badRequest("invalid alert id")
	}
	return id, nil
}

func (a *API) alertDetail(w http.ResponseWriter, r *http.Request) error {
	id, err := alertID(r)
	if err != nil {
		return err
	}
	alert, err := scanAlertRow(a.st.Pool.QueryRow(r.Context(), `SELECT `+alertColumns+` FROM alerts WHERE id=$1`, id))
	if err != nil {
		return err
	}
	type event struct {
		EventType string    `json:"event_type"`
		Actor     string    `json:"actor"`
		Note      string    `json:"note"`
		CreatedAt time.Time `json:"created_at"`
	}
	rows, err := a.st.Pool.Query(r.Context(), `SELECT event_type, actor, note, created_at FROM alert_events WHERE alert_id=$1 ORDER BY id`, id)
	if err != nil {
		return err
	}
	history, err := pgx.CollectRows(rows, pgx.RowToStructByPos[event])
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"alert": alert, "history": history})
	return nil
}

func (a *API) alertAction(target string) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := alertID(r)
		if err != nil {
			return err
		}
		var body struct {
			Actor string `json:"actor"`
			Note  string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			return badRequest("malformed body")
		}
		body.Actor = strings.TrimSpace(body.Actor)
		if body.Actor == "" || len(body.Actor) > 128 {
			return badRequest("actor is required")
		}
		// Conditional updates make these safe under concurrent operators: of two people
		// resolving at once, one succeeds and the other gets a clear conflict.
		var query, eventType string
		if target == "acknowledged" {
			eventType = "acknowledged"
			query = `UPDATE alerts SET status='acknowledged', acknowledged_at=now(), acknowledged_by=$2, updated_at=now()
				WHERE id=$1 AND status='active' RETURNING ` + alertColumns
		} else {
			eventType = "resolved"
			query = `UPDATE alerts SET status='resolved', resolved_at=now(), resolved_by=$2, updated_at=now()
				WHERE id=$1 AND status <> 'resolved' RETURNING ` + alertColumns
		}
		var alert Alert
		err = pgx.BeginFunc(r.Context(), a.st.Pool, func(tx pgx.Tx) error {
			var err error
			if alert, err = scanAlertRow(tx.QueryRow(r.Context(), query, id, body.Actor)); err != nil {
				return err
			}
			return addEvent(r.Context(), tx, id, eventType, body.Actor, body.Note)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if qerr := a.st.Pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM alerts WHERE id=$1)`, id).Scan(&exists); qerr != nil {
				return qerr
			}
			if !exists {
				return pgx.ErrNoRows
			}
			return &apiError{http.StatusConflict, "alert cannot be " + target + " from its current state"}
		}
		if err != nil {
			return err
		}
		publish(r.Context(), a.rdb, OpsEvent{Type: "alert." + eventType, At: time.Now().UTC(), Alert: &alert, Actor: body.Actor})
		writeJSON(w, 200, alert)
		return nil
	}
}

func (a *API) trends(w http.ResponseWriter, r *http.Request) error {
	hours, err := intParam(r, "hours", 24, 1, 24*30)
	if err != nil {
		return err
	}
	// Bucket width scales with the range, so a chart always has a similar point count.
	bucketMinutes := int(math.Max(1, float64(hours*60)/48))
	type point struct {
		Bucket time.Time `json:"bucket"`
		Level  string    `json:"level"`
		Count  int       `json:"count"`
	}
	rows, err := a.st.Pool.Query(r.Context(), `SELECT date_bin(make_interval(mins => $1), created_at, 'epoch'::timestamptz) AS b, level, count(*)::int
		FROM alerts WHERE created_at >= now() - make_interval(hours => $2) GROUP BY b, level ORDER BY b`, bucketMinutes, hours)
	if err != nil {
		return err
	}
	points, err := pgx.CollectRows(rows, pgx.RowToStructByPos[point])
	if err != nil {
		return err
	}
	type category struct {
		Category string   `json:"category"`
		Count    int      `json:"count"`
		Open     int      `json:"open"`
		MTTRMin  *float64 `json:"mean_minutes_to_resolve"`
	}
	rows, err = a.st.Pool.Query(r.Context(), `SELECT category, count(*)::int, (count(*) FILTER (WHERE status <> 'resolved'))::int,
		avg(extract(epoch FROM resolved_at - created_at) / 60)::float8
		FROM alerts WHERE created_at >= now() - make_interval(hours => $1) GROUP BY category ORDER BY 2 DESC`, hours)
	if err != nil {
		return err
	}
	categories, err := pgx.CollectRows(rows, pgx.RowToStructByPos[category])
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"bucket_minutes": bucketMinutes, "points": points, "categories": categories})
	return nil
}

func (a *API) listDiagnostics(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 50, 1, 200)
	if err != nil {
		return err
	}
	rows, err := a.st.Pool.Query(r.Context(), `SELECT `+diagnosticColumns+` FROM diagnostics ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return err
	}
	ds, err := pgx.CollectRows(rows, scanDiagnostic)
	if err != nil {
		return err
	}
	if ds == nil {
		ds = []Diagnostic{}
	}
	writeJSON(w, 200, map[string]any{"diagnostics": ds})
	return nil
}
