// Package diag is the diagnostic reasoning core. It is pure and deterministic:
// no Redis, Postgres or wall clock. Runtime wiring lives in services/anomaly-engine.
package diag

import "time"

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	}
	return 0
}

type Category string

const (
	CategoryCoolingFailure     Category = "battery_cooling_failure"
	CategoryThermalManagement  Category = "thermal_management_issue"
	CategoryCharging           Category = "charging_anomaly"
	CategoryExcessiveEnergy    Category = "excessive_energy_consumption"
	CategoryConnectivity       Category = "connectivity_instability"
	CategorySensorDrift        Category = "sensor_drift"
	CategoryAbnormalPowerDraw  Category = "abnormal_power_draw"
	CategorySoftwareRegression Category = "software_version_regression"
)

// Transition says what changed about a diagnosis.
type Transition string

const (
	TransitionOpened  Transition = "opened"
	TransitionUpdated Transition = "updated"
	TransitionCleared Transition = "cleared"
)

// Evidence is one piece of reasoning. Weight is its log-odds contribution, so a
// diagnosis shows exactly why it reached its confidence, including counter-evidence.
type Evidence struct {
	Signal string  `json:"signal"`
	Detail string  `json:"detail"`
	Weight float64 `json:"weight"`
}

// Diagnosis is the engine's explanation of an abnormal condition.
type Diagnosis struct {
	// Subject is a vehicle ID, or "software:<version>" for a fleet-level finding.
	Subject         string     `json:"subject"`
	Category        Category   `json:"category"`
	Transition      Transition `json:"transition"`
	Severity        Severity   `json:"severity"`
	Confidence      float64    `json:"confidence"`
	DeviationScore  float64    `json:"deviation_score"`
	Reason          string     `json:"reason"`
	LikelyCause     string     `json:"likely_cause"`
	Evidence        []Evidence `json:"evidence"`
	Actions         []string   `json:"recommended_actions"`
	SoftwareVersion string     `json:"software_version"`
	ObservedAt      time.Time  `json:"observed_at"`
}
