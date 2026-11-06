package main

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/ajtang/Intelletry/gen/telemetry/v1"
	"github.com/ajtang/Intelletry/internal/stream"
)

var (
	// ErrOverloaded means the in-process queue is full. Failing fast creates
	// backpressure; buffering without limit would convert overload into an OOM kill.
	ErrOverloaded = errors.New("ingestion queue full")
	// ErrUnavailable means Redis could not accept the batch after retries.
	ErrUnavailable = errors.New("telemetry store unavailable")
)

type publisher interface {
	Publish(ctx context.Context, entries []stream.Entry) error
}

type job struct {
	events      []*telemetryv1.TelemetryEvent
	traceParent string
	done        chan error
}

// BatcherConfig tunes group commit.
type BatcherConfig struct {
	Shards       int
	Workers      int
	QueueSize    int
	MaxEvents    int
	Linger       time.Duration
	MaxAttempts  int
	RetryBase    time.Duration
	FlushTimeout time.Duration
}

// Batcher implements group commit: concurrent requests share one Redis pipeline, and
// each is acknowledged only after that pipeline succeeds. Latency stays bounded by
// Linger while Redis round trips drop by orders of magnitude, and an acknowledgement
// still means the data is stored.
type Batcher struct {
	cfg     BatcherConfig
	pub     publisher
	queue   chan *job
	wg      sync.WaitGroup
	metrics *Metrics
}

// NewBatcher starts the flush workers.
func NewBatcher(cfg BatcherConfig, pub publisher, m *Metrics) *Batcher {
	b := &Batcher{cfg: cfg, pub: pub, queue: make(chan *job, cfg.QueueSize), metrics: m}
	for i := 0; i < cfg.Workers; i++ {
		b.wg.Add(1)
		go b.worker()
	}
	return b
}

// Submit enqueues events and waits until they are stored. If the caller's context
// ends first the caller sees an error but the flush may still succeed. That is safe:
// the client retries and the engine drops the duplicate by sequence number.
func (b *Batcher) Submit(ctx context.Context, events []*telemetryv1.TelemetryEvent, traceParent string) error {
	j := &job{events: events, traceParent: traceParent, done: make(chan error, 1)}
	select {
	case b.queue <- j:
	default:
		return ErrOverloaded
	}
	b.metrics.queueDepth.Set(float64(len(b.queue)))
	select {
	case err := <-j.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close flushes queued work. Call it only after servers have stopped accepting.
func (b *Batcher) Close() {
	close(b.queue)
	b.wg.Wait()
}

func (b *Batcher) worker() {
	defer b.wg.Done()
	for first := range b.queue {
		jobs, count := []*job{first}, len(first.events)
		timer := time.NewTimer(b.cfg.Linger)
	collect:
		for count < b.cfg.MaxEvents {
			select {
			case j, ok := <-b.queue:
				if !ok {
					break collect
				}
				jobs = append(jobs, j)
				count += len(j.events)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		err := b.flush(jobs)
		for _, j := range jobs {
			j.done <- err
		}
		b.metrics.queueDepth.Set(float64(len(b.queue)))
	}
}

func (b *Batcher) flush(jobs []*job) error {
	byShard := map[int][]*telemetryv1.TelemetryEvent{}
	trace := ""
	total := 0
	for _, j := range jobs {
		if trace == "" {
			trace = j.traceParent
		}
		for _, ev := range j.events {
			s := stream.ShardFor(ev.VehicleId, b.cfg.Shards)
			byShard[s] = append(byShard[s], ev)
			total++
		}
	}
	entries := make([]stream.Entry, 0, len(byShard))
	for shard, events := range byShard {
		payload, err := proto.Marshal(&telemetryv1.TelemetryBatch{Events: events})
		if err != nil {
			return err
		}
		entries = append(entries, stream.Entry{Shard: shard, Payload: payload, TraceParent: trace})
	}
	b.metrics.batchSize.Observe(float64(total))

	var err error
	delay := b.cfg.RetryBase
	for attempt := 1; attempt <= b.cfg.MaxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), b.cfg.FlushTimeout)
		err = b.pub.Publish(ctx, entries)
		cancel()
		if err == nil {
			return nil
		}
		b.metrics.publishRetries.Inc()
		if attempt < b.cfg.MaxAttempts {
			// Full jitter, so a Redis failover does not cause synchronized retries.
			time.Sleep(time.Duration(rand.Int63n(int64(delay)) + 1))
			delay *= 2
		}
	}
	b.metrics.publishFailures.Inc()
	return errors.Join(ErrUnavailable, err)
}
