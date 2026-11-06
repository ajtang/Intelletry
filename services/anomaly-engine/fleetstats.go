package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ajtang/Intelletry/internal/diag"
)

// FleetStats shares cohort statistics between engine pods. Each pod sees only its own
// shards, but "compared with the fleet" must mean the whole fleet. Each pod therefore
// increments per-minute histograms in Redis and rebuilds a merged snapshot from a
// rolling window. HINCRBY is commutative, so pods need no coordination.
type FleetStats struct {
	rdb     *redis.Client
	windows int

	mu    sync.Mutex
	delta map[string]diag.Histogram

	snap     atomic.Pointer[diag.Snapshot]
	findings atomic.Pointer[[]diag.VersionFinding]
	history  atomic.Pointer[map[string]float64]
}

// NewFleetStats returns an aggregator with an empty snapshot.
func NewFleetStats(rdb *redis.Client, windows int) *FleetStats {
	f := &FleetStats{rdb: rdb, windows: windows, delta: map[string]diag.Histogram{}}
	f.snap.Store(diag.BuildSnapshot(nil))
	f.findings.Store(&[]diag.VersionFinding{})
	f.history.Store(&map[string]float64{})
	return f
}

// Current returns the latest merged snapshot.
func (f *FleetStats) Current() *diag.Snapshot { return f.snap.Load() }

// Findings returns the latest software version findings.
func (f *FleetStats) Findings() []diag.VersionFinding { return *f.findings.Load() }

// History returns long-term per-vehicle residuals.
func (f *FleetStats) History() map[string]float64 { return *f.history.Load() }

// SetHistory swaps in new long-term residuals.
func (f *FleetStats) SetHistory(h map[string]float64) { f.history.Store(&h) }

// Add records samples locally until the next flush.
func (f *FleetStats) Add(samples []diag.Sample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range samples {
		key := s.Key()
		if f.delta[key] == nil {
			f.delta[key] = diag.Histogram{}
		}
		f.delta[key][s.Bin()]++
	}
}

func epochOf(t time.Time) int64 { return t.Unix() / 60 }

func epochKeys(epoch int64) string { return fmt.Sprintf("fleetstats:%d:keys", epoch) }

func epochHist(epoch int64, key string) string { return fmt.Sprintf("fleetstats:%d:h:%s", epoch, key) }

// Flush pushes local deltas to Redis. On failure the delta is merged back, so counts
// are delayed rather than lost.
func (f *FleetStats) Flush(ctx context.Context, now time.Time) error {
	f.mu.Lock()
	delta := f.delta
	f.delta = map[string]diag.Histogram{}
	f.mu.Unlock()
	if len(delta) == 0 {
		return nil
	}
	epoch := epochOf(now)
	ttl := time.Duration(f.windows+2) * time.Minute
	pipe := f.rdb.Pipeline()
	for key, hist := range delta {
		hk := epochHist(epoch, key)
		for bin, n := range hist {
			pipe.HIncrBy(ctx, hk, strconv.Itoa(bin), n)
		}
		pipe.Expire(ctx, hk, ttl)
		pipe.SAdd(ctx, epochKeys(epoch), key)
	}
	pipe.Expire(ctx, epochKeys(epoch), ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		f.mu.Lock()
		for key, hist := range delta {
			if f.delta[key] == nil {
				f.delta[key] = diag.Histogram{}
			}
			for bin, n := range hist {
				f.delta[key][bin] += n
			}
		}
		f.mu.Unlock()
		return err
	}
	return nil
}

// Refresh rebuilds the snapshot from the rolling window. On error the previous snapshot
// stays in place: slightly stale statistics are better than none.
func (f *FleetStats) Refresh(ctx context.Context, now time.Time) error {
	current := epochOf(now)
	merged := map[string]diag.Histogram{}
	for e := current - int64(f.windows) + 1; e <= current; e++ {
		keys, err := f.rdb.SMembers(ctx, epochKeys(e)).Result()
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			continue
		}
		pipe := f.rdb.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(keys))
		for i, key := range keys {
			cmds[i] = pipe.HGetAll(ctx, epochHist(e, key))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		for i, key := range keys {
			hist := merged[key]
			if hist == nil {
				hist = diag.Histogram{}
				merged[key] = hist
			}
			for b, c := range cmds[i].Val() {
				bin, err1 := strconv.Atoi(b)
				n, err2 := strconv.ParseInt(c, 10, 64)
				if err1 == nil && err2 == nil {
					hist[bin] += n
				}
			}
		}
	}
	snap := diag.BuildSnapshot(merged)
	findings := diag.FindRegressions(merged)
	for _, finding := range findings {
		snap.FlaggedVersions[finding.Version] = true
	}
	f.snap.Store(snap)
	f.findings.Store(&findings)
	return nil
}

// fleetLevelStats drops per-version keys, which are only used for regression scans.
func fleetLevelStats(in map[string]diag.Stat) map[string]diag.Stat {
	out := make(map[string]diag.Stat, len(in))
	for k, v := range in {
		if v.N > 0 && !strings.Contains(k, "|sw=") {
			out[k] = v
		}
	}
	return out
}
