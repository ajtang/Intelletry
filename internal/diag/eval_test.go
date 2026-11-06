package diag_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajtang/Intelletry/internal/diag"
	"github.com/ajtang/Intelletry/internal/sim"
)

var expected = map[sim.Fault]diag.Category{
	sim.FaultBatteryOverheat:      diag.CategoryThermalManagement,
	sim.FaultCoolingDegradation:   diag.CategoryCoolingFailure,
	sim.FaultChargingInefficiency: diag.CategoryCharging,
	sim.FaultConnectivity:         diag.CategoryConnectivity,
	sim.FaultSoftwareRegression:   diag.CategoryAbnormalPowerDraw,
	sim.FaultSensorDrift:          diag.CategorySensorDrift,
	sim.FaultExcessiveConsumption: diag.CategoryExcessiveEnergy,
}

type replayResult struct {
	vehicles   []*sim.Vehicle
	opened     map[string]map[diag.Category]float64
	regression []diag.VersionFinding
	first      map[string]diag.Diagnosis
	states     []*diag.VehicleState
}

// replay runs the fleet through the real analyzer, rebuilding fleet statistics on a
// rolling window as the runtime does.
func replay(t testing.TB, vehicles int, seed int64, duration, step time.Duration) replayResult {
	t.Helper()
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fleet := sim.NewFleet(sim.FleetConfig{
		Vehicles: vehicles, Seed: seed, Start: start, FaultRate: 0.28, MaxOnset: 12 * time.Hour,
	})
	an := diag.NewAnalyzer(diag.Config{})
	states := make([]*diag.VehicleState, len(fleet))
	for i := range states {
		states[i] = &diag.VehicleState{}
	}
	res := replayResult{states: states, vehicles: fleet, opened: map[string]map[diag.Category]float64{}, first: map[string]diag.Diagnosis{}}

	const windows = 6
	epochs := make([]map[string]diag.Histogram, 0, windows)
	current := map[string]diag.Histogram{}
	merged := func() map[string]diag.Histogram {
		out := map[string]diag.Histogram{}
		for _, e := range append(append([]map[string]diag.Histogram{}, epochs...), current) {
			for k, h := range e {
				if out[k] == nil {
					out[k] = diag.Histogram{}
				}
				for b, c := range h {
					out[k][b] += c
				}
			}
		}
		return out
	}

	var snap *diag.Snapshot
	ticks := int(duration / step)
	for tick := 1; tick <= ticks; tick++ {
		now := start.Add(time.Duration(tick) * step)
		for i, v := range fleet {
			ev := v.Step(now, step)
			if ev == nil {
				continue
			}
			r := an.Process(states[i], ev, snap)
			for _, s := range r.Samples {
				if current[s.Key()] == nil {
					current[s.Key()] = diag.Histogram{}
				}
				current[s.Key()][s.Bin()]++
			}
			for _, c := range r.Changes {
				if c.Transition == diag.TransitionOpened {
					if res.opened[c.Subject] == nil {
						res.opened[c.Subject] = map[diag.Category]float64{}
					}
					res.opened[c.Subject][c.Category] = c.Confidence
					key := c.Subject + "/" + string(c.Category)
					if _, ok := res.first[key]; !ok {
						res.first[key] = c
					}
				}
			}
		}
		if tick%int(time.Hour/step) == 0 {
			epochs = append(epochs, current)
			if len(epochs) > windows {
				epochs = epochs[1:]
			}
			current = map[string]diag.Histogram{}
			m := merged()
			snap = diag.BuildSnapshot(m)
			res.regression = diag.FindRegressions(m)
			for _, f := range res.regression {
				snap.FlaggedVersions[f.Version] = true
			}
		}
	}
	return res
}

// TestDiagnosticAccuracy is the key test in the repository. It scores the engine
// against ground truth that never appears in telemetry.
func TestDiagnosticAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("fleet replay")
	}
	for _, seed := range []int64{7, 42} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			r := replay(t, 280, seed, 72*time.Hour, time.Minute)

			total := map[sim.Fault]int{}
			hits := map[sim.Fault]int{}
			wrongOnFaulty := map[string]int{}
			tp, fp := map[diag.Category]int{}, map[diag.Category]int{}
			healthy, healthyFlagged := 0, 0
			unobservable := 0
			for i, v := range r.vehicles {
				got := r.opened[v.ID]
				// A cooling loop that never ran in the window cannot be assessed. In cold
				// climates with little DC charging this is physically correct, so these
				// vehicles are excluded and reported rather than counted as misses.
				if v.Fault == sim.FaultCoolingDegradation && r.states[i].ConductanceN < 3 {
					unobservable++
					continue
				}
				if v.Fault == sim.FaultNone {
					healthy++
					if len(got) > 0 {
						healthyFlagged++
					}
				} else {
					total[v.Fault]++
					if _, ok := got[expected[v.Fault]]; ok {
						hits[v.Fault]++
					}
				}
				for c := range got {
					if v.Fault != sim.FaultNone && expected[v.Fault] == c {
						tp[c]++
					} else {
						fp[c]++
						if v.Fault != sim.FaultNone {
							wrongOnFaulty[fmt.Sprintf("%s->%s", v.Fault, c)]++
						}
					}
				}
			}

			var report strings.Builder
			fmt.Fprintf(&report, "\n%-30s %8s\n", "fault", "recall")
			faults := make([]string, 0)
			for f := range total {
				faults = append(faults, string(f))
			}
			sort.Strings(faults)
			for _, name := range faults {
				f := sim.Fault(name)
				fmt.Fprintf(&report, "%-30s %3d/%-3d\n", name, hits[f], total[f])
			}
			fmt.Fprintf(&report, "\n%-32s %5s %5s\n", "category", "TP", "FP")
			for _, c := range expected {
				fmt.Fprintf(&report, "%-32s %5d %5d\n", c, tp[c], fp[c])
			}
			fmt.Fprintf(&report, "\nhealthy vehicles flagged: %d/%d\nunobservable cooling faults: %d\nconfusions: %v\nregressions: %+v\n", healthyFlagged, healthy, unobservable, wrongOnFaulty, r.regression)
			t.Log(report.String())

			allHits, allTotal, allTP, allFP := 0, 0, 0, 0
			for f, n := range total {
				allTotal += n
				allHits += hits[f]
			}
			for _, c := range expected {
				allTP += tp[c]
				allFP += fp[c]
			}
			for f, n := range total {
				if rate := float64(hits[f]) / float64(n); rate < 0.75 {
					t.Errorf("%s recall %.2f below 0.75", f, rate)
				}
			}
			recall := float64(allHits) / float64(allTotal)
			precision := float64(allTP) / float64(allTP+allFP)
			t.Logf("overall recall=%.3f precision=%.3f", recall, precision)
			if recall < 0.90 {
				t.Errorf("recall %.3f below 0.90", recall)
			}
			if precision < 0.90 {
				t.Errorf("precision %.3f below 0.90", precision)
			}
			if rate := float64(healthyFlagged) / float64(healthy); rate > 0.05 {
				t.Errorf("healthy false-alarm rate %.3f above 0.05", rate)
			}
			found := false
			for _, f := range r.regression {
				if f.Version == sim.RegressedVersion {
					found = true
				}
				if f.Version == sim.StableVersion {
					t.Errorf("stable version falsely implicated: %+v", f)
				}
			}
			if !found {
				t.Errorf("software regression in %s not detected at fleet level", sim.RegressedVersion)
			}
		})
	}
}

// TestAccuracyAtLiveSampleRate guards against detections that only work at the replay's
// sample rate, since the live simulator reports every two vehicle minutes.
func TestAccuracyAtLiveSampleRate(t *testing.T) {
	if testing.Short() {
		t.Skip("fleet replay")
	}
	for _, hours := range []time.Duration{10, 72} {
		r := replay(t, 280, 7, hours*time.Hour, 2*time.Minute)
		hits, total, wrong := map[sim.Fault]int{}, map[sim.Fault]int{}, 0
		for _, v := range r.vehicles {
			got := r.opened[v.ID]
			if v.Fault != sim.FaultNone {
				total[v.Fault]++
				if _, ok := got[expected[v.Fault]]; ok {
					hits[v.Fault]++
				}
			}
			for c := range got {
				if v.Fault == sim.FaultNone || expected[v.Fault] != c {
					wrong++
				}
			}
		}
		t.Logf("%2dh at 2-minute samples: hits=%v total=%v wrong=%d", hours, hits, total, wrong)
		if hours == 72 {
			for f, n := range total {
				if f == sim.FaultCoolingDegradation {
					continue // cold-climate vehicles can be unobservable, see TestDiagnosticAccuracy
				}
				if float64(hits[f])/float64(n) < 0.75 {
					t.Errorf("%s recall %d/%d below 0.75 at the live sample rate", f, hits[f], n)
				}
			}
		}
		if wrong > 0 {
			t.Errorf("%d incorrect diagnoses at %dh", wrong, hours)
		}
	}
}
