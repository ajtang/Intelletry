// Package stream defines the telemetry pipeline's Redis layout and write path.
package stream

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Group is the consumer group used by the anomaly engine.
	Group = "anomaly-engine"
	// PayloadField and TraceField are the stream entry fields.
	PayloadField = "p"
	TraceField   = "t"

	// FleetStateKey is a hash of vehicle ID to live state JSON.
	FleetStateKey = "fleet:state"
	// OpsChannel carries operational events to dashboard connections.
	OpsChannel = "ops:events"
)

// Key returns the stream key for a shard.
func Key(shard int) string { return fmt.Sprintf("telemetry:shard:%d", shard) }

// VehicleChannel is the pub/sub channel for one vehicle's live telemetry.
func VehicleChannel(id string) string { return "vehicle:live:" + id }

// TimelineKey holds recent raw samples for one vehicle.
func TimelineKey(id string) string { return "vehicle:timeline:" + id }

// StateKey holds the analyzer's persisted state for one vehicle.
func StateKey(id string) string { return "vehicle:analysis:" + id }

// ShardFor maps a vehicle to a shard. The same vehicle always lands on the same
// shard, which, combined with single-owner shards, preserves per-vehicle order.
func ShardFor(vehicleID string, shards int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(vehicleID))
	return int(h.Sum32() % uint32(shards))
}

// Entry is one encoded batch for one shard.
type Entry struct {
	Shard       int
	Payload     []byte
	TraceParent string
}

// Publisher appends batches to shard streams.
type Publisher struct {
	Client *redis.Client
	// MaxLen caps each stream. Under a long consumer outage the oldest telemetry is
	// dropped rather than letting Redis run out of memory and take the fleet state
	// down with it. The ~ trim keeps the cost of XADD constant.
	MaxLen int64
}

// Publish writes entries in one pipeline. An error may mean partial success, so the
// caller retries and the consumer de-duplicates by sequence number.
func (p *Publisher) Publish(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := p.Client.Pipeline()
	for _, e := range entries {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: Key(e.Shard),
			MaxLen: p.MaxLen,
			Approx: true,
			Values: map[string]any{PayloadField: e.Payload, TraceField: e.TraceParent},
		})
	}
	_, err := pipe.Exec(ctx)
	return err
}

// NewClient builds a Redis client tuned for fast failure, so that requests do not
// hang on a dead connection during a failover.
func NewClient(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opts.DialTimeout = 3 * time.Second
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second
	return redis.NewClient(opts), nil
}

// Depth returns unprocessed entries for a shard, using the group's lag where known.
func Depth(ctx context.Context, c *redis.Client, shard int) (int64, error) {
	groups, err := c.XInfoGroups(ctx, Key(shard)).Result()
	if err != nil {
		return 0, err
	}
	for _, g := range groups {
		if g.Name == Group {
			return g.Lag + g.Pending, nil
		}
	}
	return c.XLen(ctx, Key(shard)).Result()
}
