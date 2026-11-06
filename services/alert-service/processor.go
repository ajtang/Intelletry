package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/ajtang/Intelletry/internal/store"
	"github.com/ajtang/Intelletry/internal/stream"
)

const processorLock = 727002

var alertTitles = map[string]string{
	"battery_cooling_failure":      "Potential Battery Cooling Failure",
	"thermal_management_issue":     "Thermal Management Issue",
	"charging_anomaly":             "Charging Efficiency Degradation",
	"excessive_energy_consumption": "Unexpected Energy Consumption",
	"connectivity_instability":     "Vehicle Connectivity Instability",
	"sensor_drift":                 "Battery Sensor Drift",
	"abnormal_power_draw":          "Abnormal Power Draw",
	"software_version_regression":  "Software Version Regression Detected",
}

// OpsEvent is one entry in the real-time operations feed.
type OpsEvent struct {
	Type       string      `json:"type"`
	At         time.Time   `json:"at"`
	Alert      *Alert      `json:"alert,omitempty"`
	Diagnostic *Diagnostic `json:"diagnostic,omitempty"`
	Actor      string      `json:"actor,omitempty"`
}

// Processor turns outbox diagnostics into alerts.
//
// A transaction-scoped advisory lock admits one processor at a time across all replicas.
// That is deliberate: "opened" and "cleared" for the same problem must apply in order,
// and diagnostics are rare enough that a single active processor has ample capacity. If
// its pod dies, the lock is released with the connection and another pod continues.
type Processor struct {
	st       *store.Store
	rdb      *redis.Client
	created  *prometheus.CounterVec
	resolved prometheus.Counter
	lag      prometheus.Gauge
}

// Run processes until ctx ends.
func (p *Processor) Run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for ctx.Err() == nil {
			n, err := p.Step(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("alert processing failed", "error", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
	}
}

// Step handles one batch and returns how many diagnostics it consumed.
func (p *Processor) Step(ctx context.Context) (int, error) {
	var events []OpsEvent
	count := 0
	err := pgx.BeginFunc(ctx, p.st.Pool, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, processorLock).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT `+diagnosticColumns+` FROM diagnostics WHERE processed_at IS NULL ORDER BY id LIMIT 200`)
		if err != nil {
			return err
		}
		ds, err := pgx.CollectRows(rows, scanDiagnostic)
		if err != nil {
			return err
		}
		for i := range ds {
			evs, err := p.apply(ctx, tx, &ds[i])
			if err != nil {
				return err
			}
			events = append(events, evs...)
		}
		if len(ds) > 0 {
			ids := make([]int64, len(ds))
			for i, d := range ds {
				ids[i] = d.ID
			}
			if _, err := tx.Exec(ctx, `UPDATE diagnostics SET processed_at = now() WHERE id = ANY($1)`, ids); err != nil {
				return err
			}
		}
		count = len(ds)
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Published only after commit, so the feed never shows something that was rolled
	// back. A crash here loses a live notification, but the database remains correct and
	// dashboards reload from it.
	publish(ctx, p.rdb, events...)
	if p.lag != nil {
		var lag float64
		if err := p.st.Pool.QueryRow(ctx, `SELECT count(*) FROM diagnostics WHERE processed_at IS NULL`).Scan(&lag); err == nil {
			p.lag.Set(lag)
		}
	}
	return count, nil
}

func publish(ctx context.Context, rdb *redis.Client, events ...OpsEvent) {
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if err := rdb.Publish(ctx, stream.OpsChannel, raw).Err(); err != nil {
			slog.Debug("ops event publish failed", "error", err)
		}
	}
}

func (p *Processor) apply(ctx context.Context, tx pgx.Tx, d *Diagnostic) ([]OpsEvent, error) {
	feed := OpsEvent{Type: "diagnostic", At: d.ObservedAt, Diagnostic: d}
	if d.Transition == "cleared" {
		alert, err := scanAlertRow(tx.QueryRow(ctx, `UPDATE alerts SET status='resolved', resolved_at=now(), resolved_by='anomaly-engine',
			updated_at=now(), last_diagnostic_id=$3
			WHERE subject=$1 AND category=$2 AND status <> 'resolved' RETURNING `+alertColumns, d.Subject, d.Category, d.ID))
		if err == pgx.ErrNoRows {
			return []OpsEvent{feed}, nil // an operator already resolved it
		}
		if err != nil {
			return nil, err
		}
		if err := addEvent(ctx, tx, alert.ID, "auto_resolved", "anomaly-engine", "Condition no longer observed"); err != nil {
			return nil, err
		}
		if p.resolved != nil {
			p.resolved.Inc()
		}
		return []OpsEvent{feed, {Type: "alert.resolved", At: time.Now().UTC(), Alert: &alert, Actor: "anomaly-engine"}}, nil
	}

	title := alertTitles[d.Category]
	if title == "" {
		title = d.Category
	}
	var previous *string
	_ = tx.QueryRow(ctx, `SELECT level FROM alerts WHERE subject=$1 AND category=$2 AND status <> 'resolved'`, d.Subject, d.Category).Scan(&previous)

	// A severity increase returns an acknowledged alert to active: whoever acknowledged
	// it accepted a smaller problem than the one that now exists.
	alert, err := scanAlertRow(tx.QueryRow(ctx, `
		INSERT INTO alerts (subject, category, title, level, status, reason, likely_cause, confidence,
			recommended_actions, first_diagnostic_id, last_diagnostic_id)
		VALUES ($1,$2,$3,$4,'active',$5,$6,$7,$8,$9,$9)
		ON CONFLICT (subject, category) WHERE status <> 'resolved' DO UPDATE SET
			level = CASE WHEN level_rank(EXCLUDED.level) > level_rank(alerts.level) THEN EXCLUDED.level ELSE alerts.level END,
			status = CASE WHEN level_rank(EXCLUDED.level) > level_rank(alerts.level) THEN 'active' ELSE alerts.status END,
			reason = EXCLUDED.reason, likely_cause = EXCLUDED.likely_cause, confidence = EXCLUDED.confidence,
			recommended_actions = EXCLUDED.recommended_actions, last_diagnostic_id = EXCLUDED.last_diagnostic_id,
			occurrences = alerts.occurrences + 1, updated_at = now()
		RETURNING `+alertColumns,
		d.Subject, d.Category, title, d.Severity, d.Reason, d.LikelyCause, d.Confidence, d.Actions, d.ID))
	if err != nil {
		return nil, err
	}
	eventType, note := "updated", d.Reason
	switch {
	case previous == nil:
		eventType = "created"
		if p.created != nil {
			p.created.WithLabelValues(alert.Level, alert.Category).Inc()
		}
	case levelRank(alert.Level) > levelRank(*previous):
		eventType = "escalated"
		note = *previous + " to " + alert.Level
	}
	if err := addEvent(ctx, tx, alert.ID, eventType, "anomaly-engine", note); err != nil {
		return nil, err
	}
	return []OpsEvent{feed, {Type: "alert." + eventType, At: time.Now().UTC(), Alert: &alert, Actor: "anomaly-engine"}}, nil
}

func levelRank(l string) int {
	switch l {
	case "critical":
		return 2
	case "warning":
		return 1
	}
	return 0
}

func addEvent(ctx context.Context, tx pgx.Tx, alertID int64, eventType, actor, note string) error {
	_, err := tx.Exec(ctx, `INSERT INTO alert_events (alert_id, event_type, actor, note) VALUES ($1,$2,$3,$4)`, alertID, eventType, actor, note)
	return err
}
