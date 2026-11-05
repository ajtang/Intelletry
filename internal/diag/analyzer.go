package diag

import (
	"fmt"
	"math"
	"time"

	telemetryv1 "github.com/intelletry/ev-telemetry/gen/telemetry/v1"
)

const (
	ambientBucketC = 5
	socBucketPct   = 20
	speedBucketKph = 15
	// Thermal lag means a vehicle is not comparable to its cohort right after
	// changing modes.
	cohortDwellSec = 10 * 60
	restDwellSec   = 30 * 60
	// Above this gap, rates are meaningless and we resynchronize instead.
	maxRateGapSec = 10 * 60
	maxTrend      = 48
	trendBucket   = 30 * 60
	gapWindowSec  = 3 * 3600
	maxGaps       = 32

	openConfidence  = 0.80
	clearConfidence = 0.40
	openPersistence = 3
	clearPersist    = 6
)

// Config tunes the analyzer.
type Config struct {
	// TimeScale converts timestamps to vehicle time when the simulator runs
	// accelerated. Real fleets use 1.
	TimeScale float64
}

// ActiveDiag tracks one category's state machine for a vehicle.
type ActiveDiag struct {
	Open     bool     `json:"o"`
	Above    int      `json:"a"`
	Below    int      `json:"b"`
	Severity Severity `json:"s"`
	LastConf float64  `json:"c"`
}

// VehicleState is everything the analyzer remembers about a vehicle. It is JSON
// serializable so that another engine pod can resume after a shard handover.
type VehicleState struct {
	VehicleID string  `json:"id"`
	LastSeq   uint64  `json:"seq"`
	LastTsMs  int64   `json:"ts"`
	VTime     float64 `json:"vt"`

	Mode      string  `json:"mode"`
	ModeSince float64 `json:"ms"`
	LastBatt  float64 `json:"lb"`
	RiseRate  float64 `json:"rr"`
	Samples   int64   `json:"n"`

	// Sensor plausibility: battery vs coolant after a long, pump-off soak.
	RestDelta   float64 `json:"rd"`
	RestSamples int     `json:"rs"`

	// Sustained cooling demand under load.
	HighDemandSec float64 `json:"hd"`

	// Conductance is load-normalized heat transfer through the cooling loop:
	// (coolant-ambient) / ((battery-coolant) * duty). Temperature alone depends on
	// how hard the vehicle has worked. Conductance does not, which is what separates
	// a failing cooling loop from a vehicle that is simply generating more heat.
	Conductance  float64 `json:"cd"`
	ConductanceN int     `json:"cn"`

	// Rolling consumption over distance, from counters.
	CkptOdo    float64 `json:"co"`
	CkptEnergy float64 `json:"ce"`
	WhPerKm    float64 `json:"wk"`
	WhBaseline float64 `json:"wb"`
	CkptVTime  float64 `json:"cv"`
	WhRatio    float64 `json:"wr"`
	WhZ        float64 `json:"wz"`
	WhMedian   float64 `json:"wm"`
	WhWindows  int     `json:"wn"`
	IdleKW     float64 `json:"ik"`
	IdleSec    float64 `json:"is"`

	// Charge session, also from counters.
	SessionMode    string  `json:"sm"`
	SessionSoC     float64 `json:"ss"`
	SessionCharged float64 `json:"sc"`
	Efficiency     float64 `json:"ef"`
	EffKWh         float64 `json:"ek"`

	// Residual is battery temp minus cohort median. Fast vs slow EWMAs give
	// "compared with its own history", and the trend ring gives direction.
	ResidualFast float64   `json:"rf"`
	ResidualSlow float64   `json:"rl"`
	ResidualN    int64     `json:"rn"`
	HistResidual *float64  `json:"hr,omitempty"`
	TrendBucket  int64     `json:"tb"`
	TrendSum     float64   `json:"tsm"`
	TrendCount   int       `json:"tc"`
	Trend        []float64 `json:"tr,omitempty"`

	Gaps         []float64 `json:"g,omitempty"`
	LostSamples  int64     `json:"ls"`
	DegradedEWMA float64   `json:"de"`
	SignalEWMA   float64   `json:"sg"`

	Active map[Category]*ActiveDiag `json:"act,omitempty"`
}

// Features are per-sample derived values, surfaced to live state and rollups.
type Features struct {
	Mode            string  `json:"mode"`
	BatteryResidual float64 `json:"battery_residual"`
	BatteryZ        float64 `json:"battery_z"`
	RiseRate        float64 `json:"rise_rate_c_min"`
	WhPerKm         float64 `json:"wh_per_km"`
	Health          string  `json:"health"`
	Deviation       float64 `json:"deviation_score"`
}

// Result is the outcome of processing one event.
type Result struct {
	Duplicate bool
	Features  Features
	Samples   []Sample
	Changes   []Diagnosis
}

// Analyzer applies diagnostics. It holds no per-vehicle data, so one instance can
// safely serve every shard.
type Analyzer struct {
	cfg Config
}

// NewAnalyzer returns an analyzer.
func NewAnalyzer(cfg Config) *Analyzer {
	if cfg.TimeScale <= 0 {
		cfg.TimeScale = 1
	}
	return &Analyzer{cfg: cfg}
}

func modeOf(ev *telemetryv1.TelemetryEvent) string {
	switch ev.ChargingState {
	case telemetryv1.ChargingState_CHARGING_STATE_DC:
		return "dc"
	case telemetryv1.ChargingState_CHARGING_STATE_AC:
		return "ac"
	}
	switch {
	case ev.SpeedKph > 70:
		return "hwy"
	case ev.SpeedKph > 1:
		return "city"
	}
	return "idle"
}

func ambientCohort(mode string, ambient float64) string {
	b := int(math.Floor(ambient/ambientBucketC)) * ambientBucketC
	return fmt.Sprintf("%s|a%d", mode, b)
}

// conductanceCohort is fleet-wide because conductance is already load and ambient
// normalized, so more cohorts would only split the sample.
const conductanceCohort = "all"

func speedCohort(avgKph float64) string {
	return fmt.Sprintf("drive|v%d", int(avgKph/speedBucketKph)*speedBucketKph)
}

func socCohort(mode string, soc float64) string {
	b := int(math.Floor(soc/socBucketPct)) * socBucketPct
	return fmt.Sprintf("%s|s%d", mode, b)
}

func ewma(prev, x, dt, tau float64) float64 {
	return prev + (x-prev)*(1-math.Exp(-dt/tau))
}

// Process runs one event through feature extraction and diagnosis. Events must
// arrive in per-vehicle order, which the shard ownership model guarantees.
func (a *Analyzer) Process(st *VehicleState, ev *telemetryv1.TelemetryEvent, snap *Snapshot) Result {
	if st.Samples > 0 && ev.Seq <= st.LastSeq {
		return Result{Duplicate: true}
	}
	st.VehicleID = ev.VehicleId
	first := st.Samples == 0
	dt := 0.0
	if !first {
		dt = math.Max(0, float64(ev.TimestampMs-st.LastTsMs)/1000*a.cfg.TimeScale)
	}
	st.VTime += dt

	if !first && ev.Seq > st.LastSeq+1 {
		missed := int64(ev.Seq - st.LastSeq - 1)
		st.LostSamples += missed
		st.Gaps = append(st.Gaps, st.VTime)
	}
	for len(st.Gaps) > 0 && (st.VTime-st.Gaps[0] > gapWindowSec || len(st.Gaps) > maxGaps) {
		st.Gaps = st.Gaps[1:]
	}

	mode := modeOf(ev)
	if mode != st.Mode {
		st.Mode, st.ModeSince = mode, st.VTime
	}
	dwell := st.VTime - st.ModeSince
	comparable := dwell >= cohortDwellSec

	if !first && dt > 0 && dt <= maxRateGapSec {
		rate := (ev.BatteryTempC - st.LastBatt) / dt * 60
		st.RiseRate = ewma(st.RiseRate, rate, dt, 180)
	} else if dt > maxRateGapSec {
		st.RiseRate = 0
	}
	st.LastBatt = ev.BatteryTempC

	res := Result{}
	a.updateCounters(st, ev, snap, mode, dt, dwell, comparable, &res)
	if dt > 0 {
		degraded := 0.0
		if ev.Connectivity == telemetryv1.ConnectivityState_CONNECTIVITY_STATE_DEGRADED {
			degraded = 1
		}
		st.DegradedEWMA = ewma(st.DegradedEWMA, degraded, dt, 3600)
		st.SignalEWMA = ewma(st.SignalEWMA, ev.SignalDbm, dt, 3600)
		if ev.CoolingDemandPct >= 85 {
			st.HighDemandSec += dt
		} else {
			st.HighDemandSec = math.Max(0, st.HighDemandSec-dt)
		}
	} else if first {
		st.SignalEWMA = ev.SignalDbm
	}

	plate := ev.BatteryTempC - ev.CoolantTempC
	if mode == "idle" && dwell >= restDwellSec && ev.CoolingDemandPct < 1 {
		if st.RestSamples == 0 {
			st.RestDelta = plate
		} else {
			st.RestDelta = ewma(st.RestDelta, plate, dt, 1800)
		}
		st.RestSamples++
	}

	if ev.CoolingDemandPct >= 25 && plate >= 1.5 && ev.CoolantTempC > ev.AmbientTempC {
		g := (ev.CoolantTempC - ev.AmbientTempC) / (plate * ev.CoolingDemandPct / 100)
		if st.ConductanceN == 0 {
			st.Conductance = g
		} else {
			st.Conductance += (g - st.Conductance) * 0.25
		}
		st.ConductanceN++
		res.Samples = append(res.Samples, Sample{MetricConductance, conductanceCohort, g})
	}

	battCohort := ambientCohort(mode, ev.AmbientTempC)
	ctx := &evalContext{ev: ev, st: st, snap: snap, mode: mode, cohort: battCohort, comparable: comparable, plate: plate}
	if s, ok := snap.Lookup(MetricBatteryTemp, battCohort); ok && comparable {
		ctx.batt, ctx.battOK = s, true
		ctx.battZ = s.Z(MetricBatteryTemp, ev.BatteryTempC)
		a.updateResidual(st, ev.BatteryTempC-s.Median, dt)
		res.Features.BatteryResidual = ev.BatteryTempC - s.Median
		res.Features.BatteryZ = ctx.battZ
	}
	if comparable {
		res.Samples = append(res.Samples,
			Sample{MetricBatteryTemp, battCohort, ev.BatteryTempC},
			Sample{MetricMotorTemp, battCohort, ev.MotorTempC},
		)
		if ev.CoolingDemandPct > 1 {
			res.Samples = append(res.Samples, Sample{MetricPlateDelta, battCohort, plate})
		}
	}

	st.LastSeq, st.LastTsMs = ev.Seq, ev.TimestampMs
	st.Samples++

	res.Changes = a.evaluate(ctx, st)
	res.Features.Mode = mode
	res.Features.RiseRate = st.RiseRate
	res.Features.WhPerKm = st.WhPerKm
	res.Features.Health = st.Health()
	res.Features.Deviation = deviationScore(ctx)
	return res
}

func (a *Analyzer) updateCounters(st *VehicleState, ev *telemetryv1.TelemetryEvent, snap *Snapshot, mode string, dt, dwell float64, comparable bool, res *Result) {
	// Wh/km over at least 5 km, computed from counters so gaps cannot distort it.
	// Aerodynamic drag makes consumption strongly speed dependent, so each window is
	// compared only with windows driven at a similar average speed.
	if st.CkptOdo == 0 {
		st.CkptOdo, st.CkptEnergy, st.CkptVTime = ev.OdometerKm, ev.EnergyConsumedKwh, st.VTime
	}
	if km := ev.OdometerKm - st.CkptOdo; km >= 5 {
		wh := (ev.EnergyConsumedKwh - st.CkptEnergy) * 1000 / km
		elapsed := st.VTime - st.CkptVTime
		st.CkptOdo, st.CkptEnergy, st.CkptVTime = ev.OdometerKm, ev.EnergyConsumedKwh, st.VTime
		avgKph := 0.0
		if elapsed > 0 {
			avgKph = km / (elapsed / 3600)
		}
		// Windows that include parked time or a data gap fold in idle drain.
		if (mode == "city" || mode == "hwy") && avgKph >= 20 && wh > 0 && wh < 2000 {
			if st.WhPerKm == 0 {
				st.WhPerKm = wh
			} else {
				st.WhPerKm += (wh - st.WhPerKm) * 0.35
			}
			if st.WhBaseline == 0 {
				st.WhBaseline = st.WhPerKm
			} else {
				st.WhBaseline += (st.WhPerKm - st.WhBaseline) * 0.01
			}
			cohort := speedCohort(avgKph)
			if s, ok := snap.Lookup(MetricWhPerKm, cohort); ok && s.Median > 0 {
				ratio, z := wh/s.Median, s.Z(MetricWhPerKm, wh)
				if st.WhWindows == 0 {
					st.WhRatio, st.WhZ = ratio, z
				} else {
					st.WhRatio += (ratio - st.WhRatio) * 0.25
					st.WhZ += (z - st.WhZ) * 0.25
				}
				st.WhMedian = s.Median
				st.WhWindows++
			}
			res.Samples = append(res.Samples,
				Sample{MetricWhPerKm, cohort, wh},
				Sample{MetricWhPerKm, versionCohortPrefix + ev.SoftwareVersion, wh},
			)
		}
	}
	if mode == "idle" && dwell >= cohortDwellSec && ev.PackPowerKw >= 0 {
		if st.IdleSec == 0 {
			st.IdleKW = ev.PackPowerKw
		} else {
			st.IdleKW = ewma(st.IdleKW, ev.PackPowerKw, dt, 1200)
		}
		st.IdleSec += dt
		res.Samples = append(res.Samples,
			Sample{MetricIdleKW, ambientCohort("idle", ev.AmbientTempC), ev.PackPowerKw},
			Sample{MetricIdleKW, versionCohortPrefix + ev.SoftwareVersion, ev.PackPowerKw},
		)
	}
	if mode == "ac" || mode == "dc" {
		if st.SessionMode != mode {
			st.SessionMode, st.SessionSoC, st.SessionCharged = mode, ev.SocPct, ev.EnergyChargedKwh
		}
		if delivered := ev.EnergyChargedKwh - st.SessionCharged; delivered >= 3 && ev.PackCapacityKwh > 0 {
			// Subtract nothing for aux draw: aux is small and affects the whole
			// fleet equally, so it does not change the comparison.
			gained := (ev.SocPct - st.SessionSoC) / 100 * ev.PackCapacityKwh
			st.Efficiency = gained / delivered
			st.EffKWh = delivered
		}
		if comparable {
			res.Samples = append(res.Samples, Sample{MetricChargeKW, socCohort(mode, ev.SocPct), ev.ChargePowerKw})
		}
	} else {
		st.SessionMode = ""
	}
}

func (a *Analyzer) updateResidual(st *VehicleState, residual, dt float64) {
	if st.ResidualN == 0 {
		st.ResidualFast, st.ResidualSlow = residual, residual
	} else {
		st.ResidualFast = ewma(st.ResidualFast, residual, dt, 2*3600)
		st.ResidualSlow = ewma(st.ResidualSlow, residual, dt, 72*3600)
	}
	st.ResidualN++
	bucket := int64(st.VTime / trendBucket)
	if bucket != st.TrendBucket && st.TrendCount > 0 {
		st.Trend = append(st.Trend, st.TrendSum/float64(st.TrendCount))
		if len(st.Trend) > maxTrend {
			st.Trend = st.Trend[len(st.Trend)-maxTrend:]
		}
		st.TrendSum, st.TrendCount = 0, 0
	}
	st.TrendBucket = bucket
	st.TrendSum += residual
	st.TrendCount++
}

// TrendPerDay fits a least-squares line to residuals and returns °C per day and r².
func TrendPerDay(values []float64) (slope, r2 float64, ok bool) {
	n := len(values)
	if n < 8 {
		return 0, 0, false
	}
	var sx, sy, sxx, sxy, syy float64
	for i, y := range values {
		x := float64(i)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
		syy += y * y
	}
	fn := float64(n)
	den := fn*sxx - sx*sx
	if den == 0 {
		return 0, 0, false
	}
	b := (fn*sxy - sx*sy) / den
	vy := fn*syy - sy*sy
	if vy <= 0 {
		return 0, 0, false
	}
	corr := (fn*sxy - sx*sy) / math.Sqrt(den*vy)
	return b * (24 * 3600 / trendBucket), corr * corr, true
}

// Health summarizes the worst active diagnosis.
func (st *VehicleState) Health() string {
	worst := ""
	for _, d := range st.Active {
		if !d.Open {
			continue
		}
		switch d.Severity {
		case SeverityCritical:
			return "critical"
		case SeverityWarning:
			worst = "warning"
		}
	}
	if worst != "" {
		return worst
	}
	return "healthy"
}

// SetHistory supplies a long-term baseline loaded from Postgres rollups.
func (st *VehicleState) SetHistory(residual float64) { st.HistResidual = &residual }

func ts(ev *telemetryv1.TelemetryEvent) time.Time { return time.UnixMilli(ev.TimestampMs).UTC() }
