package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Atomic token bucket. Running in Redis makes the limit global across replicas, so
// scaling out ingestion does not silently multiply a credential's allowance.
var tokenBucket = redis.NewScript(`
local tokens = tonumber(redis.call('HGET', KEYS[1], 't') or ARGV[2])
local last = tonumber(redis.call('HGET', KEYS[1], 'l') or ARGV[3])
local rate, burst, now, cost = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
tokens = math.min(burst, tokens + math.max(0, now - last) / 1000 * rate)
local allowed = 0
if tokens >= cost then tokens = tokens - cost; allowed = 1 end
redis.call('HSET', KEYS[1], 't', tokens, 'l', now)
redis.call('PEXPIRE', KEYS[1], 60000)
return allowed`)

// Limiter enforces events per second per credential. If Redis is unreachable it falls
// back to a per-pod bucket: losing global accuracy during an outage is better than
// rejecting all telemetry, or having no limit at all.
type Limiter struct {
	client *redis.Client
	rate   float64
	burst  float64
	mu     sync.Mutex
	local  map[string]*localBucket
}

type localBucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter creates a limiter.
func NewLimiter(c *redis.Client, rate, burst float64) *Limiter {
	return &Limiter{client: c, rate: rate, burst: burst, local: map[string]*localBucket{}}
}

// Allow reports whether a credential may submit cost events.
func (l *Limiter) Allow(ctx context.Context, credential string, cost int) bool {
	if float64(cost) > l.burst {
		return false
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	res, err := tokenBucket.Run(ctx, l.client, []string{"ratelimit:" + credential}, l.rate, l.burst, now.UnixMilli(), cost).Int()
	if err == nil {
		return res == 1
	}
	slog.Debug("rate limiter falling back to local bucket", "error", err)
	return l.allowLocal(credential, cost, now)
}

func (l *Limiter) allowLocal(credential string, cost int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.local[credential]
	if b == nil {
		b = &localBucket{tokens: l.burst, last: now}
		l.local[credential] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < float64(cost) {
		return false
	}
	b.tokens -= float64(cost)
	return true
}
