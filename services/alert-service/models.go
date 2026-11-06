package main

import (
	"time"

	"github.com/jackc/pgx/v5"
)

// Alert is an operational alert.
type Alert struct {
	ID             int64      `json:"id"`
	Subject        string     `json:"subject"`
	Category       string     `json:"category"`
	Title          string     `json:"title"`
	Level          string     `json:"level"`
	Status         string     `json:"status"`
	Reason         string     `json:"reason"`
	LikelyCause    string     `json:"likely_cause"`
	Confidence     float64    `json:"confidence"`
	Actions        []string   `json:"recommended_actions"`
	Occurrences    int        `json:"occurrences"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
	AcknowledgedBy *string    `json:"acknowledged_by"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	ResolvedBy     *string    `json:"resolved_by"`
}

const alertColumns = `id, subject, category, title, level, status, reason, likely_cause, confidence,
	recommended_actions, occurrences, created_at, updated_at, acknowledged_at, acknowledged_by, resolved_at, resolved_by`

func scanAlertRow(row pgx.Row) (Alert, error) {
	var a Alert
	err := row.Scan(&a.ID, &a.Subject, &a.Category, &a.Title, &a.Level, &a.Status, &a.Reason, &a.LikelyCause, &a.Confidence,
		&a.Actions, &a.Occurrences, &a.CreatedAt, &a.UpdatedAt, &a.AcknowledgedAt, &a.AcknowledgedBy, &a.ResolvedAt, &a.ResolvedBy)
	return a, err
}

func scanAlert(row pgx.CollectableRow) (Alert, error) { return scanAlertRow(row) }

// EvidenceItem is one piece of diagnostic reasoning.
type EvidenceItem struct {
	Signal string  `json:"signal"`
	Detail string  `json:"detail"`
	Weight float64 `json:"weight"`
}

// Diagnostic is a stored engine diagnosis.
type Diagnostic struct {
	ID              int64          `json:"id"`
	Subject         string         `json:"subject"`
	Category        string         `json:"category"`
	Transition      string         `json:"transition"`
	Severity        string         `json:"severity"`
	Confidence      float64        `json:"confidence"`
	DeviationScore  float64        `json:"deviation_score"`
	Reason          string         `json:"reason"`
	LikelyCause     string         `json:"likely_cause"`
	Evidence        []EvidenceItem `json:"evidence"`
	Actions         []string       `json:"recommended_actions"`
	SoftwareVersion string         `json:"software_version"`
	ObservedAt      time.Time      `json:"observed_at"`
}

const diagnosticColumns = `id, subject, category, transition, severity, confidence, deviation_score, reason, likely_cause,
	evidence, recommended_actions, software_version, observed_at`

func scanDiagnostic(row pgx.CollectableRow) (Diagnostic, error) {
	var d Diagnostic
	err := row.Scan(&d.ID, &d.Subject, &d.Category, &d.Transition, &d.Severity, &d.Confidence, &d.DeviationScore, &d.Reason,
		&d.LikelyCause, &d.Evidence, &d.Actions, &d.SoftwareVersion, &d.ObservedAt)
	return d, err
}
