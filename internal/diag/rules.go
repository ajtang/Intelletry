package diag

import (
	"fmt"
	"math"

	telemetryv1 "github.com/intelletry/ev-telemetry/gen/telemetry/v1"
)

type evalContext struct {
	ev         *telemetryv1.TelemetryEvent
	st         *VehicleState
	snap       *Snapshot
	mode       string
	cohort     string
	comparable bool
	plate      float64
	batt       Stat
	battOK     bool
	battZ      float64
}

// candidate is the output of one diagnostic rule. Each rule starts from a skeptical
// prior and accumulates log-odds, so independent evidence compounds and contradicting
// evidence subtracts. The confidence is a scored belief, not a calibrated probability.
//
// observable is false when the current state says nothing either way, for example
// charge efficiency while driving. Absence of evidence must not clear a diagnosis.
type candidate struct {
	category   Category
	logOdds    float64
	observable bool
	severity   Severity
	reason     string
	cause      string
	actions    []string
	evidence   []Evidence
}

func (c *candidate) add(signal string, weight float64, format string, args ...any) {
	c.logOdds += weight
	c.evidence = append(c.evidence, Evidence{Signal: signal, Detail: fmt.Sprintf(format, args...), Weight: weight})
}

func (c *evalContext) driftSuspected() bool {
	return c.st.RestSamples >= 3 && math.Abs(c.st.RestDelta) >= 3
}

// fleetReason renders the headline comparison, for example "32% above the fleet".
func fleetReason(label string, value, median float64, unit, cohort string, n int64, z float64) string {
	pct := 0.0
	if median != 0 {
		pct = (value - median) / math.Abs(median) * 100
	}
	dir := "above"
	if pct < 0 {
		dir, pct = "below", -pct
	}
	return fmt.Sprintf("%s of %.1f%s is %.0f%% %s the fleet median of %.1f%s for vehicles %s (n=%d, %.1fσ).",
		label, value, unit, pct, dir, median, unit, cohortLabel(cohort), n, math.Abs(z))
}

func ruleCooling(c *evalContext) candidate {
	out := candidate{category: CategoryCoolingFailure, logOdds: -3.5, severity: SeverityWarning,
		cause: "Cooling system degradation: heat is not leaving the pack despite full cooling demand, so suspect low coolant, a weak pump or a fouled cold plate.",
		actions: []string{
			"Check coolant level and inspect for leaks",
			"Run the coolant pump flow diagnostic",
			"Limit DC fast charging until cooling is verified",
			"Schedule thermal-system service",
		}}
	ev := c.ev
	st, statOK := c.snap.Lookup(MetricConductance, conductanceCohort)
	out.observable = statOK && c.st.ConductanceN >= 3 && ev.CoolingDemandPct >= 25
	if !out.observable {
		return out
	}
	g := c.st.Conductance
	z := st.Z(MetricConductance, g)
	if z <= -3 {
		pct := (1 - g/st.Median) * 100
		out.add("cooling_conductance", 3.5, "cooling-loop heat transfer is %.0f%% below the fleet median (%.2f vs %.2f, %.1fσ) after normalizing for load", pct, g, st.Median, -z)
		out.reason = fmt.Sprintf("Cooling loop is moving %.0f%% less heat than the fleet median at the same load (n=%d, %.1fσ).", pct, st.N, -z)
	} else if g >= st.Median*0.75 {
		out.add("cooling_conductance", -3.0, "cooling-loop heat transfer is normal (%.2f vs fleet %.2f), so any heat problem lies elsewhere", g, st.Median)
	}
	if g < st.Median*0.6 {
		out.add("cooling_conductance_severe", 1.5, "loss of more than 40%% is too large to be measurement noise")
	}
	if c.battOK && c.comparable && c.battZ >= 2.5 {
		out.add("battery_temp", 1.5, "%.1f°C is %.1fσ above the cohort median of %.1f°C", ev.BatteryTempC, c.battZ, c.batt.Median)
		out.reason = fleetReason("Battery temperature", ev.BatteryTempC, c.batt.Median, "°C", c.cohort, c.batt.N, c.battZ) + " " + out.reason
	}
	if ev.CoolingDemandPct >= 85 {
		out.add("cooling_demand", 0.5, "thermal system commanded %.0f%% cooling", ev.CoolingDemandPct)
	}
	if slope, r2, ok := TrendPerDay(c.st.Trend); ok && slope >= 1 && r2 >= 0.35 {
		out.add("trend", 1.0, "temperature residual is rising %.1f°C/day (r²=%.2f): gradual degradation rather than an event", slope, r2)
	}
	if base := c.historical(); base != nil && c.st.ResidualFast-*base >= 3 {
		out.add("history", 1.0, "running %.1f°C hotter relative to the fleet than this vehicle's own baseline of %+.1f°C", c.st.ResidualFast-*base, *base)
	}
	if c.driftSuspected() {
		out.add("sensor_plausibility", -6.0, "battery and coolant disagree by %.1f°C at rest, so the reading itself is suspect", c.st.RestDelta)
	}
	if ev.BatteryTempC >= 55 {
		out.severity = SeverityCritical
	}
	return out
}

func ruleThermal(c *evalContext) candidate {
	out := candidate{category: CategoryThermalManagement, logOdds: -3.5, severity: SeverityWarning,
		cause: "Abnormal internal heat generation in the pack: temperature is rising faster than the electrical load can explain, so suspect a cell-level fault.",
		actions: []string{
			"Inspect BMS cell-level temperature and voltage logs",
			"Stop DC fast charging and limit vehicle power",
			"Contact the driver if temperature continues to rise",
			"Prioritize a pack inspection",
		}}
	ev := c.ev
	out.observable = c.mode != "idle" || ev.BatteryTempC >= 40
	if !out.observable {
		return out
	}
	if ev.BatteryTempC >= 58 {
		out.add("battery_temp", 5.0, "%.1f°C exceeds the 58°C critical limit", ev.BatteryTempC)
		out.severity = SeverityCritical
	} else if ev.BatteryTempC >= 50 {
		out.add("battery_temp", 2.5, "%.1f°C exceeds the 50°C warning limit", ev.BatteryTempC)
	}
	current := math.Abs(ev.PackPowerKw) * 1000 / 400
	expected := current * current * packResistanceOhm / 1000 / 500 * 60 // °C/min from I²R alone
	if c.st.RiseRate >= 0.8 && c.st.RiseRate-expected >= 0.6 {
		out.add("rise_rate", 3.5, "rising %.2f°C/min when the load explains about %.2f°C/min", c.st.RiseRate, expected)
		if out.reason == "" {
			out.reason = fmt.Sprintf("Battery temperature is climbing %.2f°C/min at %.1f°C, well beyond what the current electrical load explains.", c.st.RiseRate, ev.BatteryTempC)
		}
	}
	if c.st.RiseRate-expected >= 1.2 {
		out.add("rise_rate_severe", 1.5, "the unexplained rise of %.2f°C/min is far outside normal thermal behaviour", c.st.RiseRate-expected)
	}
	if st, ok := c.snap.Lookup(MetricConductance, conductanceCohort); ok && c.st.ConductanceN >= 3 && c.st.Conductance >= st.Median*0.75 && ev.CoolingDemandPct >= 25 {
		out.add("cooling_conductance", 0.5, "cooling loop is transferring heat normally, so the source is excess heat rather than lost cooling")
	}
	if c.battOK && c.battZ >= 4 {
		out.add("battery_temp_fleet", 1.5, "%.1fσ above vehicles in the same conditions", c.battZ)
		if out.reason == "" {
			out.reason = fleetReason("Battery temperature", ev.BatteryTempC, c.batt.Median, "°C", c.cohort, c.batt.N, c.battZ)
		}
	}
	if ev.CoolingDemandPct >= 85 {
		out.add("cooling_demand", 0.5, "cooling is commanded at %.0f%%", ev.CoolingDemandPct)
	}
	if c.driftSuspected() {
		out.add("sensor_plausibility", -5.0, "battery and coolant disagree by %.1f°C at rest, so the reading itself is suspect", c.st.RestDelta)
	}
	return out
}

func ruleSensorDrift(c *evalContext) candidate {
	out := candidate{category: CategorySensorDrift, logOdds: -3.5, severity: SeverityInfo,
		cause: "Battery temperature sensor drift: after a long soak with the pump off, the battery and coolant should read the same, and they do not.",
		actions: []string{
			"Compare the sensor against adjacent BMS cell probes",
			"Exclude this sensor from thermal analytics until verified",
			"Schedule sensor replacement or recalibration",
		}}
	out.observable = c.st.RestSamples >= 3 && c.mode == "idle"
	if !out.observable {
		return out
	}
	d := c.st.RestDelta
	if math.Abs(d) >= 3 {
		out.add("rest_delta", 5.0, "battery reads %+.1f°C against coolant after a long soak; physically they should converge", d)
		out.reason = fmt.Sprintf("Battery temperature sensor disagrees with coolant by %+.1f°C while parked with no thermal load.", d)
	}
	if math.Abs(d) >= 6 {
		out.add("rest_delta_size", 1.0, "the %.1f°C offset is too large to be residual heat", math.Abs(d))
		out.severity = SeverityWarning
	}
	if c.ev.CoolingDemandPct < 1 {
		out.add("cooling_demand", 0.5, "the thermal system sees no need to cool, which matches a false reading")
	}
	return out
}

func ruleCharging(c *evalContext) candidate {
	out := candidate{category: CategoryCharging, logOdds: -3.5, severity: SeverityWarning,
		cause: "Charging efficiency degradation: energy delivered is not reaching the pack, so suspect on-board charger losses or a high-resistance charge path.",
		actions: []string{
			"Inspect the charge port and contactors for heat damage",
			"Run the on-board charger diagnostic",
			"Check whether the loss follows this vehicle across charging sites",
		}}
	ev := c.ev
	out.observable = (c.mode == "ac" || c.mode == "dc") && c.st.EffKWh >= 3
	if !out.observable {
		return out
	}
	eff := c.st.Efficiency
	if eff < 0.84 {
		out.add("charge_efficiency", 4.0, "%.0f%% of %.1f kWh delivered reached the pack; healthy vehicles keep about 90%%", eff*100, c.st.EffKWh)
		out.reason = fmt.Sprintf("Charge efficiency is %.0f%% this session: %.1f kWh delivered, only %.1f kWh stored.", eff*100, c.st.EffKWh, eff*c.st.EffKWh)
	}
	if eff < 0.76 {
		out.add("charge_efficiency_severe", 1.0, "losses are large enough to rule out measurement noise")
	}
	cohort := socCohort(c.mode, ev.SocPct)
	if s, ok := c.snap.Lookup(MetricChargeKW, cohort); ok && c.comparable {
		if z := s.Z(MetricChargeKW, ev.ChargePowerKw); z <= -3 {
			out.add("charge_power", 1.0, "%.0f kW vs fleet median %.0f kW for %s", ev.ChargePowerKw, s.Median, cohortLabel(cohort))
		}
	}
	if ev.BatteryTempC >= 46 {
		out.add("thermal_derate", -2.0, "at %.1f°C the BMS derates charging, so low power may be expected behaviour", ev.BatteryTempC)
	}
	return out
}

func ruleEnergy(c *evalContext) candidate {
	out := candidate{category: CategoryExcessiveEnergy, logOdds: -3.5, severity: SeverityWarning,
		cause: "Mechanical drag or drivetrain loss: consumption is high across trips in normal conditions, so suspect a dragging brake, wheel alignment or tire pressure.",
		actions: []string{
			"Check tire pressures and wheel alignment",
			"Inspect brakes for drag and check wheel-end temperatures",
			"Review motor inverter efficiency logs",
		}}
	out.observable = (c.mode == "city" || c.mode == "hwy") && c.st.WhPerKm > 0
	if !out.observable {
		return out
	}
	if c.st.WhWindows < 4 {
		out.observable = false
		return out
	}
	if c.st.WhZ >= 2 && c.st.WhRatio >= 1.2 {
		out.add("wh_per_km", 4.0, "%.0f%% above vehicles at the same speed (%.1fσ) across recent driving", (c.st.WhRatio-1)*100, c.st.WhZ)
		out.reason = fmt.Sprintf("Energy use is %.0f%% above the fleet median for vehicles at similar speeds (about %.0f Wh/km vs %.0f Wh/km).", (c.st.WhRatio-1)*100, c.st.WhRatio*c.st.WhMedian, c.st.WhMedian)
	}
	if c.st.WhBaseline > 0 && c.st.WhPerKm/c.st.WhBaseline >= 1.15 {
		out.add("history", 1.5, "up %.0f%% on this vehicle's own baseline of %.0f Wh/km", (c.st.WhPerKm/c.st.WhBaseline-1)*100, c.st.WhBaseline)
	}
	if st, ok := c.snap.Lookup(MetricMotorTemp, c.cohort); ok && c.comparable && st.Z(MetricMotorTemp, c.ev.MotorTempC) >= 2 {
		out.add("motor_temp", 0.5, "motor is running warmer than its cohort, consistent with extra load")
	}
	if c.snap != nil && c.snap.FlaggedVersions[c.ev.SoftwareVersion] {
		out.add("software", -2.0, "software %s is under fleet-level investigation, which may explain the increase", c.ev.SoftwareVersion)
	}
	return out
}

func rulePowerDraw(c *evalContext) candidate {
	out := candidate{category: CategoryAbnormalPowerDraw, logOdds: -3.5, severity: SeverityWarning,
		cause: "Parasitic load while parked: a module is staying awake or an accessory is drawing power.",
		actions: []string{
			"Capture a sleep-state trace to find the awake module",
			"Check recently installed accessories",
			"Warn the driver about accelerated parked battery drain",
		}}
	out.observable = c.mode == "idle" && c.st.IdleSec >= 1200
	if !out.observable {
		return out
	}
	kw := c.st.IdleKW
	if kw >= 0.3 {
		out.add("idle_power", 3.5, "parked draw of %.2f kW while asleep vehicles draw about 0.04 kW", kw)
		out.reason = fmt.Sprintf("Vehicle is drawing %.2f kW while parked, losing about %.1f kWh per day.", kw, kw*24)
	}
	if s, ok := c.snap.Lookup(MetricIdleKW, ambientCohort("idle", c.ev.AmbientTempC)); ok {
		if z := s.Z(MetricIdleKW, kw); z >= 4 {
			out.add("idle_power_fleet", 1.5, "%.1fσ above parked vehicles in the same conditions (median %.2f kW)", z, s.Median)
		}
	}
	if c.snap != nil && c.snap.FlaggedVersions[c.ev.SoftwareVersion] {
		out.add("software", 1.5, "software %s shows this deviation across the fleet", c.ev.SoftwareVersion)
		out.cause = fmt.Sprintf("Software regression in %s: the power deviation follows the release across many vehicles and is not specific to this one.", c.ev.SoftwareVersion)
		out.actions = []string{
			"Roll back or hotfix software " + c.ev.SoftwareVersion,
			"Do not dispatch service for this vehicle unless the draw persists after the fix",
		}
	}
	return out
}

func ruleConnectivity(c *evalContext) candidate {
	out := candidate{category: CategoryConnectivity, logOdds: -3.5, severity: SeverityInfo,
		cause: "Intermittent telematics link: repeated data loss suggests a failing modem, antenna or SIM rather than ordinary coverage holes.",
		actions: []string{
			"Check telematics control unit logs for modem resets",
			"Inspect the antenna connection",
			"Check whether vehicles on the same route show the same pattern",
		},
		observable: c.st.Samples > 30}
	gaps := len(c.st.Gaps)
	if gaps >= 4 {
		out.add("data_gaps", 3.5, "%d telemetry gaps in the last 3 hours, %d samples lost overall", gaps, c.st.LostSamples)
		out.reason = fmt.Sprintf("Telemetry dropped out %d times in the last 3 hours.", gaps)
	}
	if gaps >= 7 {
		out.add("data_gaps_high", 1.5, "this outage frequency is well beyond normal coverage loss")
		out.severity = SeverityWarning
	}
	if c.st.DegradedEWMA >= 0.15 {
		out.add("degraded_link", 1.0, "link reported degraded %.0f%% of the time", c.st.DegradedEWMA*100)
	}
	if c.st.SignalEWMA <= -105 {
		out.add("signal", 0.5, "average signal of %.0f dBm points to coverage or antenna issues", c.st.SignalEWMA)
	}
	return out
}

func (c *evalContext) historical() *float64 {
	if c.st.HistResidual != nil {
		return c.st.HistResidual
	}
	if c.st.ResidualN >= 200 {
		return &c.st.ResidualSlow
	}
	return nil
}

// packResistanceOhm is the nominal pack resistance assumed by the heat model. A real
// deployment would look this up per pack variant.
const packResistanceOhm = 0.10

var rules = []func(*evalContext) candidate{
	ruleCooling, ruleThermal, ruleSensorDrift, ruleCharging, ruleEnergy, rulePowerDraw, ruleConnectivity,
}

func (a *Analyzer) evaluate(c *evalContext, st *VehicleState) []Diagnosis {
	var changes []Diagnosis
	for _, rule := range rules {
		cand := rule(c)
		if !cand.observable {
			continue
		}
		if st.Active == nil {
			st.Active = map[Category]*ActiveDiag{}
		}
		d := st.Active[cand.category]
		if d == nil {
			d = &ActiveDiag{}
			st.Active[cand.category] = d
		}
		conf := sigmoid(cand.logOdds)
		switch {
		case conf >= openConfidence:
			d.Above++
			d.Below = 0
		case conf < clearConfidence:
			d.Below++
			d.Above = 0
		default:
			d.Above = 0
		}
		switch {
		case !d.Open && d.Above >= openPersistence:
			d.Open, d.Severity, d.LastConf = true, cand.severity, conf
			changes = append(changes, makeDiagnosis(c, cand, conf, TransitionOpened))
		case d.Open && d.Below >= clearPersist:
			d.Open, d.Above, d.Below = false, 0, 0
			out := makeDiagnosis(c, cand, conf, TransitionCleared)
			out.Severity = d.Severity
			out.Reason = "Condition no longer observed."
			changes = append(changes, out)
		case d.Open && conf >= openConfidence &&
			(severityRank(cand.severity) > severityRank(d.Severity) || math.Abs(conf-d.LastConf) >= 0.10):
			d.Severity = maxSeverity(d.Severity, cand.severity)
			d.LastConf = conf
			out := makeDiagnosis(c, cand, conf, TransitionUpdated)
			out.Severity = d.Severity
			changes = append(changes, out)
		}
	}
	return changes
}

func maxSeverity(a, b Severity) Severity {
	if severityRank(b) > severityRank(a) {
		return b
	}
	return a
}

func makeDiagnosis(c *evalContext, cand candidate, conf float64, t Transition) Diagnosis {
	reason := cand.reason
	if reason == "" && len(cand.evidence) > 0 {
		reason = cand.evidence[0].Detail
	}
	return Diagnosis{
		Subject:         c.ev.VehicleId,
		Category:        cand.category,
		Transition:      t,
		Severity:        cand.severity,
		Confidence:      math.Round(conf*1000) / 1000,
		DeviationScore:  deviationScore(c),
		Reason:          reason,
		LikelyCause:     cand.cause,
		Evidence:        cand.evidence,
		Actions:         cand.actions,
		SoftwareVersion: c.ev.SoftwareVersion,
		ObservedAt:      ts(c.ev),
	}
}

// deviationScore maps a robust z-score to 0-100 for ranking vehicles.
func deviationScore(c *evalContext) float64 {
	return math.Round(100 * (1 - math.Exp(-math.Max(0, math.Abs(c.battZ))/4)))
}
