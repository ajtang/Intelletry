package diag

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Metric names a fleet-compared signal.
type Metric string

const (
	MetricBatteryTemp Metric = "battery_temp"
	MetricMotorTemp   Metric = "motor_temp"
	MetricPlateDelta  Metric = "plate_delta"
	MetricWhPerKm     Metric = "wh_per_km"
	MetricIdleKW      Metric = "idle_kw"
	MetricChargeKW    Metric = "charge_kw"
	MetricConductance Metric = "cooling_conductance"
)

// binWidth sets histogram resolution. Histograms rather than running means are the
// core tradeoff here: they merge additively across engine pods through Redis, and
// they yield a median and MAD, so a cohort of faulty vehicles cannot drag the
// baseline toward itself the way it would drag a mean.
func binWidth(m Metric) float64 {
	switch m {
	case MetricBatteryTemp, MetricPlateDelta:
		return 0.5
	case MetricMotorTemp:
		return 1
	case MetricWhPerKm:
		return 5
	case MetricIdleKW:
		return 0.02
	case MetricChargeKW:
		return 2
	case MetricConductance:
		return 0.05
	}
	return 1
}

// madFloor stops a very uniform cohort from producing enormous scores on tiny,
// physically meaningless differences.
func madFloor(m Metric) float64 {
	switch m {
	case MetricBatteryTemp:
		return 1.0
	case MetricMotorTemp:
		return 2.0
	case MetricPlateDelta:
		return 0.6
	case MetricWhPerKm:
		return 12
	case MetricIdleKW:
		return 0.03
	case MetricChargeKW:
		return 5
	case MetricConductance:
		return 0.06
	}
	return 1
}

// Sample is one observation contributed to the fleet aggregate.
type Sample struct {
	Metric Metric
	Cohort string
	Value  float64
}

// Key is the aggregate key: metric|cohort.
func (s Sample) Key() string { return string(s.Metric) + "|" + s.Cohort }

// Bin returns the histogram bucket for the sample.
func (s Sample) Bin() int { return int(math.Floor(s.Value / binWidth(s.Metric))) }

// Stat is a robust summary of a cohort.
type Stat struct {
	Median float64
	MAD    float64
	N      int64
}

// Z returns a robust z-score: the distance from the median in estimated standard
// deviations, with the MAD floored per metric.
func (s Stat) Z(m Metric, x float64) float64 {
	mad := math.Max(s.MAD, madFloor(m))
	return (x - s.Median) / (1.4826 * mad)
}

// Histogram is a sparse, mergeable bucket count.
type Histogram map[int]int64

// Summarize computes median and MAD from a histogram, using bin midpoints.
func Summarize(m Metric, h Histogram) Stat {
	var total int64
	bins := make([]int, 0, len(h))
	for b, c := range h {
		if c > 0 {
			bins = append(bins, b)
			total += c
		}
	}
	if total == 0 {
		return Stat{}
	}
	sort.Ints(bins)
	w := binWidth(m)
	value := func(b int) float64 { return (float64(b) + 0.5) * w }

	var cum int64
	var median float64
	for _, b := range bins {
		cum += h[b]
		if cum*2 >= total {
			median = value(b)
			break
		}
	}
	type dev struct {
		d float64
		c int64
	}
	devs := make([]dev, 0, len(bins))
	for _, b := range bins {
		devs = append(devs, dev{math.Abs(value(b) - median), h[b]})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].d < devs[j].d })
	cum = 0
	var mad float64
	for _, d := range devs {
		cum += d.c
		if cum*2 >= total {
			mad = d.d
			break
		}
	}
	return Stat{Median: median, MAD: mad, N: total}
}

// Snapshot is an immutable view of fleet statistics, swapped atomically.
type Snapshot struct {
	Stats map[string]Stat
	// FlaggedVersions holds software versions with a fleet-level regression.
	FlaggedVersions map[string]bool
}

// MinCohortSamples is the minimum evidence before a fleet comparison is trusted.
const MinCohortSamples = 40

// Lookup returns cohort statistics if there are enough samples to trust them.
func (s *Snapshot) Lookup(m Metric, cohort string) (Stat, bool) {
	if s == nil {
		return Stat{}, false
	}
	st, ok := s.Stats[string(m)+"|"+cohort]
	if !ok || st.N < MinCohortSamples {
		return Stat{}, false
	}
	return st, true
}

// BuildSnapshot converts merged histograms into a snapshot.
func BuildSnapshot(hists map[string]Histogram) *Snapshot {
	snap := &Snapshot{Stats: make(map[string]Stat, len(hists)), FlaggedVersions: map[string]bool{}}
	for key, h := range hists {
		metric, _, ok := strings.Cut(key, "|")
		if !ok {
			continue
		}
		snap.Stats[key] = Summarize(Metric(metric), h)
	}
	return snap
}

// VersionFinding is the result of a fleet-level software comparison.
type VersionFinding struct {
	Version     string
	Metric      Metric
	Median      float64
	PeerMedian  float64
	EffectPct   float64
	Samples     int64
	PeerSamples int64
}

const (
	versionCohortPrefix = "sw="
	minVersionSamples   = 150
)

// FindRegressions compares each software version with all other versions on
// metrics tagged by version. Requiring a large population and a large effect keeps
// a handful of individually faulty vehicles from implicating a whole release.
func FindRegressions(hists map[string]Histogram) []VersionFinding {
	byMetric := map[Metric]map[string]Histogram{}
	for key, h := range hists {
		metric, cohort, ok := strings.Cut(key, "|")
		if !ok || !strings.HasPrefix(cohort, versionCohortPrefix) {
			continue
		}
		m := Metric(metric)
		if byMetric[m] == nil {
			byMetric[m] = map[string]Histogram{}
		}
		byMetric[m][strings.TrimPrefix(cohort, versionCohortPrefix)] = h
	}
	var out []VersionFinding
	for metric, versions := range byMetric {
		if len(versions) < 2 {
			continue
		}
		for version, h := range versions {
			peers := Histogram{}
			for other, oh := range versions {
				if other == version {
					continue
				}
				for b, c := range oh {
					peers[b] += c
				}
			}
			st, peer := Summarize(metric, h), Summarize(metric, peers)
			if st.N < minVersionSamples || peer.N < minVersionSamples || peer.Median <= 0 {
				continue
			}
			effect := (st.Median - peer.Median) / peer.Median * 100
			threshold := 25.0
			if metric == MetricIdleKW {
				threshold = 100 // idle draw is small, so relative changes are noisier
			}
			if effect >= threshold && peer.Z(metric, st.Median) >= 3 {
				out = append(out, VersionFinding{version, metric, st.Median, peer.Median, effect, st.N, peer.N})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EffectPct > out[j].EffectPct })
	return out
}

// RegressionDiagnosis turns version findings into a fleet-level diagnosis.
func RegressionDiagnosis(version string, findings []VersionFinding) Diagnosis {
	ev := make([]Evidence, 0, len(findings))
	top := findings[0]
	for _, f := range findings {
		ev = append(ev, Evidence{
			Signal: string(f.Metric),
			Detail: fmt.Sprintf("median %.3g on %s vs %.3g on other versions (+%.0f%%, n=%d vs %d)",
				f.Median, f.Version, f.PeerMedian, f.EffectPct, f.Samples, f.PeerSamples),
			Weight: 2,
		})
	}
	return Diagnosis{
		Subject:        "software:" + version,
		Category:       CategorySoftwareRegression,
		Severity:       SeverityWarning,
		Confidence:     sigmoid(0.5 + 2*float64(len(findings))),
		DeviationScore: math.Min(100, top.EffectPct/4),
		Reason: fmt.Sprintf("%s on software %s is %.0f%% above other versions under the same conditions (%d samples).",
			metricLabel(top.Metric), version, top.EffectPct, top.Samples),
		LikelyCause: fmt.Sprintf("Regression introduced in software %s, since the deviation follows the release rather than individual vehicles.", version),
		Evidence:    ev,
		Actions: []string{
			"Pause the staged rollout of " + version,
			"Compare power-state traces between " + version + " and the previous release",
			"Prepare a rollback or hotfix for affected vehicles",
		},
		SoftwareVersion: version,
	}
}

func metricLabel(m Metric) string {
	switch m {
	case MetricIdleKW:
		return "Parked power draw"
	case MetricWhPerKm:
		return "Energy consumption"
	}
	return string(m)
}

// AmbientCohortLabel renders a cohort key such as "dc|a20" for humans.
func cohortLabel(cohort string) string {
	mode, rest, _ := strings.Cut(cohort, "|")
	label := map[string]string{
		"idle": "parked", "city": "city driving", "hwy": "highway driving",
		"ac": "AC charging", "dc": "DC fast charging",
	}[mode]
	if label == "" {
		label = mode
	}
	if strings.HasPrefix(rest, "a") {
		if n, err := strconv.Atoi(rest[1:]); err == nil {
			return fmt.Sprintf("%s at %d–%d°C ambient", label, n, n+ambientBucketC)
		}
	}
	if strings.HasPrefix(rest, "s") {
		if n, err := strconv.Atoi(rest[1:]); err == nil {
			return fmt.Sprintf("%s at %d–%d%% SoC", label, n, n+socBucketPct)
		}
	}
	return label
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }
