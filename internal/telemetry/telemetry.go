// Package telemetry holds validation and the live-state contract shared across services.
package telemetry

import (
	"fmt"
	"math"
	"regexp"
	"time"

	telemetryv1 "github.com/ajtang/Intelletry/gen/telemetry/v1"
)

const (
	// Vehicles legitimately buffer data through coverage gaps, so old timestamps are
	// allowed. Future timestamps beyond clock skew are always a bug.
	maxEventAge = 24 * time.Hour
	maxSkew     = 5 * time.Minute
)

var vehicleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func inRange(name string, v, lo, hi float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < lo || v > hi {
		return fmt.Errorf("%s %v outside [%v, %v]", name, v, lo, hi)
	}
	return nil
}

// Validate rejects structurally impossible events. Limits are physical bounds, not
// normal ranges: a 90°C battery reading must reach the engine, because it may be the
// most important event of the day. Only a value no sensor could report is rejected.
func Validate(ev *telemetryv1.TelemetryEvent, now time.Time) error {
	if !vehicleIDPattern.MatchString(ev.GetVehicleId()) {
		return fmt.Errorf("invalid vehicle_id %q", ev.GetVehicleId())
	}
	if ev.GetSeq() == 0 {
		return fmt.Errorf("seq must be positive")
	}
	ts := time.UnixMilli(ev.GetTimestampMs())
	if ts.Before(now.Add(-maxEventAge)) {
		return fmt.Errorf("timestamp older than %s", maxEventAge)
	}
	if ts.After(now.Add(maxSkew)) {
		return fmt.Errorf("timestamp more than %s in the future", maxSkew)
	}
	if ev.GetChargingState() == telemetryv1.ChargingState_CHARGING_STATE_UNSPECIFIED {
		return fmt.Errorf("charging_state is required")
	}
	if ev.GetConnectivity() == telemetryv1.ConnectivityState_CONNECTIVITY_STATE_UNSPECIFIED {
		return fmt.Errorf("connectivity is required")
	}
	if ev.GetSoftwareVersion() == "" {
		return fmt.Errorf("software_version is required")
	}
	checks := []struct {
		name      string
		v, lo, hi float64
	}{
		{"soc_pct", ev.GetSocPct(), 0, 100},
		{"battery_temp_c", ev.GetBatteryTempC(), -60, 200},
		{"motor_temp_c", ev.GetMotorTempC(), -60, 300},
		{"coolant_temp_c", ev.GetCoolantTempC(), -60, 200},
		{"ambient_temp_c", ev.GetAmbientTempC(), -70, 70},
		{"cooling_demand_pct", ev.GetCoolingDemandPct(), 0, 100},
		{"speed_kph", ev.GetSpeedKph(), 0, 400},
		{"latitude", ev.GetLatitude(), -90, 90},
		{"longitude", ev.GetLongitude(), -180, 180},
		{"heading_deg", ev.GetHeadingDeg(), 0, 360},
		{"odometer_km", ev.GetOdometerKm(), 0, 5e6},
		{"charge_power_kw", ev.GetChargePowerKw(), 0, 1000},
		{"pack_power_kw", ev.GetPackPowerKw(), -1500, 1500},
		{"energy_consumed_kwh", ev.GetEnergyConsumedKwh(), 0, 1e9},
		{"energy_charged_kwh", ev.GetEnergyChargedKwh(), 0, 1e9},
		{"pack_capacity_kwh", ev.GetPackCapacityKwh(), 1, 500},
	}
	for _, c := range checks {
		if err := inRange(c.name, c.v, c.lo, c.hi); err != nil {
			return err
		}
	}
	return nil
}

// LiveState is one vehicle's current state as stored in Redis and sent to dashboards.
type LiveState struct {
	VehicleID        string  `json:"vehicle_id"`
	TimestampMs      int64   `json:"timestamp_ms"`
	Model            string  `json:"model"`
	Region           string  `json:"region"`
	SoftwareVersion  string  `json:"software_version"`
	SocPct           float64 `json:"soc_pct"`
	BatteryTempC     float64 `json:"battery_temp_c"`
	MotorTempC       float64 `json:"motor_temp_c"`
	CoolantTempC     float64 `json:"coolant_temp_c"`
	AmbientTempC     float64 `json:"ambient_temp_c"`
	CoolingDemandPct float64 `json:"cooling_demand_pct"`
	SpeedKph         float64 `json:"speed_kph"`
	Latitude         float64 `json:"latitude"`
	Longitude        float64 `json:"longitude"`
	HeadingDeg       float64 `json:"heading_deg"`
	OdometerKm       float64 `json:"odometer_km"`
	ChargingState    string  `json:"charging_state"`
	ChargePowerKw    float64 `json:"charge_power_kw"`
	PackPowerKw      float64 `json:"pack_power_kw"`
	Connectivity     string  `json:"connectivity"`
	SignalDbm        float64 `json:"signal_dbm"`
	Mode             string  `json:"mode"`
	Health           string  `json:"health"`
	Deviation        float64 `json:"deviation_score"`
	BatteryResidual  float64 `json:"battery_residual"`
	WhPerKm          float64 `json:"wh_per_km"`
	RiseRate         float64 `json:"rise_rate_c_min"`
}

// TimelineSample is a compact point in a vehicle's recent history. It is deliberately
// smaller than LiveState because Redis holds one list per vehicle.
type TimelineSample struct {
	T       int64   `json:"t"`
	Battery float64 `json:"b"`
	Motor   float64 `json:"m"`
	Ambient float64 `json:"a"`
	SoC     float64 `json:"s"`
	Speed   float64 `json:"v"`
	Power   float64 `json:"p"`
}
