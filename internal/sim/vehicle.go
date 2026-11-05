// Package sim is a deterministic physical model of an EV fleet. It has no I/O,
// so the same code drives the live simulator and the offline diagnostic evaluation.
package sim

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	telemetryv1 "github.com/intelletry/ev-telemetry/gen/telemetry/v1"
)

// Fault identifies an injected failure. Faults are ground truth: they are never
// placed on the wire, so diagnostics must infer them from telemetry alone.
type Fault string

const (
	FaultNone                 Fault = ""
	FaultBatteryOverheat      Fault = "battery_overheat"
	FaultCoolingDegradation   Fault = "cooling_degradation"
	FaultChargingInefficiency Fault = "charging_inefficiency"
	FaultConnectivity         Fault = "connectivity_intermittent"
	FaultSoftwareRegression   Fault = "software_regression"
	FaultSensorDrift          Fault = "sensor_drift"
	FaultExcessiveConsumption Fault = "excessive_consumption"
)

// AllFaults lists every injectable fault.
var AllFaults = []Fault{
	FaultBatteryOverheat, FaultCoolingDegradation, FaultChargingInefficiency,
	FaultConnectivity, FaultSoftwareRegression, FaultSensorDrift, FaultExcessiveConsumption,
}

const (
	StableVersion     = "2026.40.2"
	RegressedVersion  = "2026.44.1"
	internalStepSec   = 10.0
	packHeatCapacity  = 500.0 // kJ/K
	coolantPlateKW    = 1.5   // kW/K at full pump duty
	radiatorKW        = 1.2   // kW/K
	passiveLeakKW     = 0.05  // kW/K
	packResistanceOhm = 0.10
	packVoltage       = 400.0
	motorHeatCapacity = 60.0 // kJ/K
)

type phase int

const (
	phaseParked phase = iota
	phaseDriving
	phaseChargingAC
	phaseChargingDC
)

type region struct {
	name          string
	lat, lon      float64
	meanAmbientC  float64
	dailySwingC   float64
	utcOffsetHour float64
}

var regions = []region{
	{"us-west", 37.55, -122.15, 19, 7, -8},
	{"us-southwest", 33.45, -112.05, 32, 8, -7},
	{"us-midwest", 42.35, -83.25, 8, 6, -5},
	{"eu-central", 48.15, 11.55, 11, 5, 1},
}

// Vehicle holds true physical state. Reported values may differ from it, for
// example when a temperature sensor drifts.
type Vehicle struct {
	ID, VIN, Model, Software string
	Fault                    Fault
	FaultOnset               time.Time

	rng       *rand.Rand
	region    region
	capacity  float64
	phase     phase
	phaseEnd  time.Time
	targetSoC float64

	soc, batteryC, motorC, coolantC, ambientC, coolingDuty float64
	speed, lat, lon, heading, odometer                     float64
	destLat, destLon                                       float64
	consumedKWh, chargedKWh, packPowerKW, chargePowerKW    float64
	ambientNoise                                           float64

	seq         uint64
	outageUntil time.Time
	degraded    bool
	signal      float64
	overheatOn  bool
	overheatEnd time.Time
}

// NewVehicle creates a vehicle whose behaviour is fully determined by seed.
func NewVehicle(index int, seed int64, start time.Time, fault Fault, onset time.Time) *Vehicle {
	rng := rand.New(rand.NewSource(seed))
	r := regions[index%len(regions)]
	models := []struct {
		name string
		kwh  float64
	}{{"R-Series LR", 92}, {"R-Series SR", 75}, {"T-Van", 110}}
	m := models[rng.Intn(len(models))]
	v := &Vehicle{
		ID:         fmt.Sprintf("EV-%05d", index+1),
		VIN:        fmt.Sprintf("7EVTEL%011d", 10_000_000+index),
		Model:      m.name,
		Software:   StableVersion,
		Fault:      fault,
		FaultOnset: onset,
		rng:        rng,
		region:     r,
		capacity:   m.kwh,
		soc:        35 + rng.Float64()*60,
		odometer:   2_000 + rng.Float64()*60_000,
		lat:        r.lat + (rng.Float64()-0.5)*0.5,
		lon:        r.lon + (rng.Float64()-0.5)*0.6,
		signal:     -75 - rng.Float64()*20,
	}
	v.ambientC = v.ambientAt(start)
	v.batteryC = v.ambientC + rng.Float64()*2
	v.motorC = v.ambientC
	v.coolantC = v.batteryC
	v.phase = phaseParked
	v.phaseEnd = start.Add(time.Duration(rng.Intn(90)) * time.Minute)
	return v
}

func (v *Vehicle) ambientAt(t time.Time) float64 {
	u := t.UTC()
	localHour := float64(u.Hour()) + float64(u.Minute())/60 + v.region.utcOffsetHour
	return v.region.meanAmbientC + v.region.dailySwingC*math.Sin((localHour-9)/24*2*math.Pi)
}

func (v *Vehicle) faultActive(now time.Time) bool {
	return v.Fault != FaultNone && !now.Before(v.FaultOnset)
}

// faultProgress ramps from 0 to 1 over 12 vehicle hours, because real degradation
// is gradual rather than a step change.
func (v *Vehicle) faultProgress(now time.Time) float64 {
	if !v.faultActive(now) {
		return 0
	}
	return math.Min(1, now.Sub(v.FaultOnset).Hours()/12)
}

// Step advances the physics by dt and returns a sample, or nil if the vehicle is
// out of coverage. Lost samples still consume a sequence number, which is how the
// backend can distinguish lost data from an idle vehicle.
func (v *Vehicle) Step(now time.Time, dt time.Duration) *telemetryv1.TelemetryEvent {
	total := dt.Seconds()
	steps := int(math.Ceil(total / internalStepSec))
	if steps < 1 {
		steps = 1
	}
	h := total / float64(steps)
	for i := 0; i < steps; i++ {
		t := now.Add(-dt + time.Duration(float64(i+1)*h*float64(time.Second)))
		v.integrate(t, h)
	}
	v.seq++
	v.updateConnectivity(now, dt)
	if now.Before(v.outageUntil) {
		return nil
	}
	return v.sample(now)
}

func (v *Vehicle) integrate(t time.Time, h float64) {
	v.ambientNoise += (-v.ambientNoise/1800 + (v.rng.NormFloat64()*0.02)/math.Sqrt(h)) * h
	v.ambientC = v.ambientAt(t) + v.ambientNoise
	v.transition(t)
	if v.Fault == FaultSoftwareRegression && v.faultActive(t) {
		v.Software = RegressedVersion
	}

	aux := 0.04
	drive := 0.0
	v.chargePowerKW = 0
	chargeIntoPack := 0.0
	switch v.phase {
	case phaseDriving:
		v.updateSpeed(h)
		drive = v.tractionKW()
		aux = 0.35 + 0.025*math.Abs(v.ambientC-21)
		v.moveAlongRoute(h)
	case phaseChargingAC, phaseChargingDC:
		v.speed = 0
		v.chargePowerKW = v.chargerKW()
		efficiency := 0.93
		if v.Fault == FaultChargingInefficiency {
			efficiency -= 0.26 * v.faultProgress(t)
			v.chargePowerKW *= 1 - 0.25*v.faultProgress(t)
		}
		chargeIntoPack = v.chargePowerKW * efficiency
		aux = 0.25
	default:
		v.speed = 0
	}
	if v.Fault == FaultSoftwareRegression && v.faultActive(t) {
		if v.phase == phaseDriving {
			aux += 0.4
		} else {
			aux += 0.85 // the regression keeps the compute module awake
		}
	}
	if v.Fault == FaultExcessiveConsumption && v.phase == phaseDriving {
		drive *= 1 + 0.45*v.faultProgress(t)
	}

	discharge := drive + aux
	v.packPowerKW = discharge - chargeIntoPack
	v.consumedKWh += math.Max(discharge, 0) * h / 3600
	v.chargedKWh += v.chargePowerKW * h / 3600
	v.soc = clamp(v.soc-(v.packPowerKW*h/3600)/v.capacity*100, 0, 100)
	v.odometer += v.speed * h / 3600
	v.integrateThermal(t, h, drive)
}

func (v *Vehicle) integrateThermal(t time.Time, h, drive float64) {
	current := v.packPowerKW * 1000 / packVoltage
	heat := current * current * packResistanceOhm / 1000
	if v.Fault == FaultBatteryOverheat && v.faultActive(t) {
		heat += v.overheatHeat(t)
	}

	// Thermostat: active thermal management when loaded, passive when parked.
	threshold := math.Max(30, v.ambientC+3)
	if v.phase == phaseChargingDC { // vehicles pre-emptively cool during fast charging
		threshold = math.Max(24, v.ambientC+2)
	}
	duty := clamp((v.batteryC-threshold)/8, 0, 1)
	if v.phase == phaseParked && v.batteryC < 40 {
		duty = 0
	}
	v.coolingDuty = duty

	effectiveness := 1.0
	if v.Fault == FaultCoolingDegradation {
		effectiveness = 1 - 0.75*v.faultProgress(t)
	}
	rejected := 0.0
	if duty > 0.01 {
		plate := coolantPlateKW * math.Max(duty, 0.2) * effectiveness
		rejected = (v.batteryC - v.ambientC) / (1/plate + 1/radiatorKW)
		if rejected < 0 {
			rejected = 0
		}
		v.coolantC = v.ambientC + rejected/radiatorKW
	} else {
		v.coolantC += (v.batteryC - 0.2 - v.coolantC) * (1 - math.Exp(-h/120))
	}
	leak := (v.batteryC - v.ambientC) * passiveLeakKW
	v.batteryC += (heat - rejected - leak) * h / packHeatCapacity

	motorHeat := math.Abs(drive) * 0.06
	motorLoss := (v.motorC - v.ambientC) * 0.09
	v.motorC += (motorHeat - motorLoss) * h / motorHeatCapacity
}

// overheatHeat models a cell-level exothermic fault: repeating load-triggered
// episodes of internal heating that do not scale with pack current.
func (v *Vehicle) overheatHeat(t time.Time) float64 {
	if v.overheatOn && t.After(v.overheatEnd) {
		v.overheatOn = false
	}
	if !v.overheatOn && v.phase != phaseParked && v.rng.Float64() < 0.004 {
		v.overheatOn = true
		v.overheatEnd = t.Add(time.Duration(15+v.rng.Intn(25)) * time.Minute)
	}
	if v.overheatOn {
		return 14 + 10*v.faultProgress(t)
	}
	return 0
}

func (v *Vehicle) transition(t time.Time) {
	switch v.phase {
	case phaseParked:
		if t.After(v.phaseEnd) {
			if v.soc < 25 {
				v.startCharge(t, v.rng.Float64() < 0.5)
			} else {
				v.startTrip(t)
			}
		}
	case phaseDriving:
		if v.soc < 22 {
			v.startCharge(t, true)
		} else if t.After(v.phaseEnd) {
			if v.soc < 45 && v.rng.Float64() < 0.65 {
				v.startCharge(t, v.rng.Float64() < 0.6)
			} else {
				v.park(t)
			}
		}
	case phaseChargingAC, phaseChargingDC:
		if v.soc >= v.targetSoC {
			v.park(t)
		}
	}
}

func (v *Vehicle) startTrip(t time.Time) {
	v.phase = phaseDriving
	v.phaseEnd = t.Add(time.Duration(10+v.rng.Intn(55)) * time.Minute)
	v.pickDestination()
}

func (v *Vehicle) park(t time.Time) {
	v.phase = phaseParked
	minutes := 20 + v.rng.ExpFloat64()*130
	if v.rng.Float64() < 0.25 {
		minutes += 240 + v.rng.Float64()*360
	}
	v.phaseEnd = t.Add(time.Duration(minutes) * time.Minute)
}

func (v *Vehicle) startCharge(_ time.Time, dc bool) {
	if dc {
		v.phase = phaseChargingDC
		v.targetSoC = 75 + v.rng.Float64()*10
	} else {
		v.phase = phaseChargingAC
		v.targetSoC = 85 + v.rng.Float64()*12
	}
}

func (v *Vehicle) updateSpeed(h float64) {
	target := 45.0
	if int(v.odometer/8)%2 == 0 {
		target = 105
	}
	target += v.rng.NormFloat64() * 8
	v.speed += (target - v.speed) * (1 - math.Exp(-h/45))
	v.speed = math.Max(v.speed, 0)
}

func (v *Vehicle) tractionKW() float64 {
	ms := v.speed / 3.6
	rolling := 0.012 * 2300 * 9.81 * ms
	aero := 0.5 * 1.2 * 0.68 * ms * ms * ms
	return (rolling + aero) / 1000 / 0.88
}

func (v *Vehicle) chargerKW() float64 {
	if v.phase == phaseChargingAC {
		return 11
	}
	power := 170.0
	if v.soc > 50 {
		power = 170 - (v.soc-50)*3
	}
	if v.batteryC > 46 { // the BMS derates hot packs
		power *= clamp(1-(v.batteryC-46)/14, 0.25, 1)
	}
	return math.Max(power, 25)
}

func (v *Vehicle) pickDestination() {
	v.destLat = v.region.lat + (v.rng.Float64()-0.5)*0.7
	v.destLon = v.region.lon + (v.rng.Float64()-0.5)*0.9
}

func (v *Vehicle) moveAlongRoute(h float64) {
	dLat := v.destLat - v.lat
	dLon := (v.destLon - v.lon) * math.Cos(v.lat*math.Pi/180)
	distKm := math.Hypot(dLat, dLon) * 111
	if distKm < 0.5 {
		v.pickDestination()
		return
	}
	v.heading = math.Mod(math.Atan2(dLon, dLat)*180/math.Pi+360, 360)
	stepKm := math.Min(v.speed*h/3600, distKm)
	f := stepKm / distKm
	v.lat += (v.destLat - v.lat) * f
	v.lon += (v.destLon - v.lon) * f
}

func (v *Vehicle) updateConnectivity(now time.Time, dt time.Duration) {
	perHour := 0.015 // tunnels and parking garages affect healthy vehicles too
	if v.Fault == FaultConnectivity && v.faultActive(now) {
		perHour = 4.5
	}
	v.degraded = false
	if !now.Before(v.outageUntil) && v.rng.Float64() < 1-math.Exp(-perHour*dt.Hours()) {
		v.outageUntil = now.Add(time.Duration(2+v.rng.Intn(10)) * time.Minute)
	}
	if v.Fault == FaultConnectivity && v.faultActive(now) && v.rng.Float64() < 0.35 {
		v.degraded = true
	}
}

func (v *Vehicle) sample(now time.Time) *telemetryv1.TelemetryEvent {
	reportedBattery := v.batteryC
	if v.Fault == FaultSensorDrift && v.faultActive(now) {
		reportedBattery += math.Min(14, now.Sub(v.FaultOnset).Hours()*0.7)
	}
	state := telemetryv1.ChargingState_CHARGING_STATE_NONE
	switch v.phase {
	case phaseChargingAC:
		state = telemetryv1.ChargingState_CHARGING_STATE_AC
	case phaseChargingDC:
		state = telemetryv1.ChargingState_CHARGING_STATE_DC
	}
	conn := telemetryv1.ConnectivityState_CONNECTIVITY_STATE_ONLINE
	signal := v.signal + v.rng.NormFloat64()*2
	if v.degraded {
		conn = telemetryv1.ConnectivityState_CONNECTIVITY_STATE_DEGRADED
		signal -= 18
	}
	noise := func(sd float64) float64 { return v.rng.NormFloat64() * sd }
	return &telemetryv1.TelemetryEvent{
		EventId:           fmt.Sprintf("%s-%d", v.ID, v.seq),
		VehicleId:         v.ID,
		Seq:               v.seq,
		TimestampMs:       now.UnixMilli(),
		Vin:               v.VIN,
		Model:             v.Model,
		PackCapacityKwh:   v.capacity,
		SoftwareVersion:   v.Software,
		Region:            v.region.name,
		SocPct:            round(v.soc, 2),
		BatteryTempC:      round(reportedBattery+noise(0.08), 2),
		MotorTempC:        round(v.motorC+noise(0.15), 2),
		CoolantTempC:      round(v.coolantC+noise(0.08), 2),
		AmbientTempC:      round(v.ambientC+noise(0.05), 2),
		CoolingDemandPct:  round(v.coolingDuty*100, 1),
		SpeedKph:          round(v.speed, 1),
		Latitude:          round(v.lat, 6),
		Longitude:         round(v.lon, 6),
		HeadingDeg:        round(v.heading, 1),
		OdometerKm:        round(v.odometer, 3),
		ChargingState:     state,
		ChargePowerKw:     round(v.chargePowerKW, 2),
		PackPowerKw:       round(v.packPowerKW, 3),
		EnergyConsumedKwh: round(v.consumedKWh, 4),
		EnergyChargedKwh:  round(v.chargedKWh, 4),
		Connectivity:      conn,
		SignalDbm:         round(signal, 1),
	}
}

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

func round(x float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(x*p) / p
}
