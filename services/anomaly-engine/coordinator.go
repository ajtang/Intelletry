package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	membersKey  = "engine:members"
	leaderShard = 0
)

func leaseKey(shard int) string { return fmt.Sprintf("engine:lease:%d", shard) }

var renewLease = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 0`)

var releaseLease = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

// Coordinator assigns stream shards to engine pods using Redis leases.
//
// Why not a plain consumer group across pods? A group hands entries to whichever
// consumer is free, so two samples from one vehicle could be processed concurrently
// and out of order, and every diagnostic here is stateful per vehicle. A shard has one
// owner at a time, which gives per-vehicle ordering and a single-writer in-memory state
// without per-vehicle locks. Consumer groups are still used on each shard for pending
// tracking and crash recovery.
type Coordinator struct {
	rdb      *redis.Client
	id       string
	shards   int
	leaseTTL time.Duration
	tick     time.Duration
	start    func(ctx context.Context, shard int) (done <-chan struct{}, setFenced func())

	mu    sync.Mutex
	owned map[int]*ownedShard
}

type ownedShard struct {
	cancel    context.CancelFunc
	done      <-chan struct{}
	setFenced func()
}

// IsLeader reports whether this pod owns the leader shard. Tying leadership to a lease
// avoids a second election mechanism for fleet-level jobs.
func (c *Coordinator) IsLeader() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.owned[leaderShard]
	return ok
}

// OwnedCount returns the number of shards held.
func (c *Coordinator) OwnedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.owned)
}

// Run balances shards until ctx ends, then hands them off cleanly.
func (c *Coordinator) Run(ctx context.Context) {
	c.reconcile(ctx)
	ticker := time.NewTicker(c.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.releaseAll()
			return
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

func (c *Coordinator) reconcile(ctx context.Context) {
	now := time.Now()
	// A member expires with the same TTL as its leases. A longer window would leave a dead
	// pod counted after its leases lapse, so survivors would refuse the orphaned shards.
	expiry := now.Add(-c.leaseTTL).UnixMilli()
	pipe := c.rdb.Pipeline()
	pipe.ZAdd(ctx, membersKey, redis.Z{Score: float64(now.UnixMilli()), Member: c.id})
	pipe.ZRemRangeByScore(ctx, membersKey, "-inf", fmt.Sprint(expiry))
	count := pipe.ZCard(ctx, membersKey)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("coordinator heartbeat failed", "error", err)
	}
	members := max(1, int(count.Val()))
	desired := int(math.Ceil(float64(c.shards) / float64(members)))

	// Renew first. A failed renewal could mean Redis is briefly unavailable, in which
	// case the worker keeps its shard until the lease has truly been taken. Only an
	// explicit mismatch means another pod owns it.
	c.mu.Lock()
	held := make([]int, 0, len(c.owned))
	for s := range c.owned {
		held = append(held, s)
	}
	c.mu.Unlock()
	for _, s := range held {
		res, err := renewLease.Run(ctx, c.rdb, []string{leaseKey(s)}, c.id, c.leaseTTL.Milliseconds()).Int()
		if err == nil && res == 0 {
			slog.Warn("lease lost, fencing worker", "shard", s)
			c.stop(s, true)
		}
	}

	// Shed one shard per tick, so rebalancing is gradual rather than a thundering herd.
	if c.OwnedCount() > desired {
		c.mu.Lock()
		victim := -1
		for s := range c.owned {
			if s != leaderShard && (victim == -1 || s > victim) {
				victim = s
			}
		}
		if victim == -1 {
			for s := range c.owned {
				victim = s
			}
		}
		c.mu.Unlock()
		c.stop(victim, false)
		_ = releaseLease.Run(context.Background(), c.rdb, []string{leaseKey(victim)}, c.id).Err()
		slog.Info("shard released for rebalance", "shard", victim, "members", members)
		return
	}

	if c.OwnedCount() >= desired {
		return
	}
	for _, s := range rand.Perm(c.shards) {
		if c.OwnedCount() >= desired {
			break
		}
		c.mu.Lock()
		_, mine := c.owned[s]
		c.mu.Unlock()
		if mine {
			continue
		}
		ok, err := c.rdb.SetNX(ctx, leaseKey(s), c.id, c.leaseTTL).Result()
		if err != nil {
			slog.Warn("lease acquire failed", "shard", s, "error", err)
			return
		}
		if !ok {
			continue
		}
		workerCtx, cancel := context.WithCancel(ctx)
		done, setFenced := c.start(workerCtx, s)
		c.mu.Lock()
		c.owned[s] = &ownedShard{cancel: cancel, done: done, setFenced: setFenced}
		c.mu.Unlock()
		slog.Info("shard acquired", "shard", s, "members", members)
	}
}

// stop cancels a shard worker and waits for it. A fenced worker has lost its lease, so
// it must not write its state snapshot over the new owner's.
func (c *Coordinator) stop(shard int, fenced bool) {
	c.mu.Lock()
	o := c.owned[shard]
	delete(c.owned, shard)
	c.mu.Unlock()
	if o == nil {
		return
	}
	if fenced {
		o.setFenced()
	}
	o.cancel()
	<-o.done
}

func (c *Coordinator) releaseAll() {
	c.mu.Lock()
	held := make([]int, 0, len(c.owned))
	for s := range c.owned {
		held = append(held, s)
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range held {
		c.stop(s, false)
		_ = releaseLease.Run(ctx, c.rdb, []string{leaseKey(s)}, c.id).Err()
	}
	_ = c.rdb.ZRem(ctx, membersKey, c.id).Err()
}
