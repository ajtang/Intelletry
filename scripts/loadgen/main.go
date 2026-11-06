// Command loadgen drives telemetry-ingestion over gRPC and reports throughput and
// latency. Run it through scripts/bench.sh, which starts a throwaway Redis and service.
//
// Open-loop runs measure latency from each request's intended send time, so a stalled
// server is not hidden by a stalled client (coordinated omission). After each run the
// shard streams are read back and counted, to check acknowledgements against storage.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	telemetryv1 "github.com/ajtang/Intelletry/gen/telemetry/v1"
	"github.com/ajtang/Intelletry/internal/stream"
)

const (
	vehicles    = 10000
	connections = 8
)

type fleet struct {
	ids  []string
	seqs []atomic.Uint64
	next atomic.Uint64
}

func newFleet() *fleet {
	f := &fleet{ids: make([]string, vehicles), seqs: make([]atomic.Uint64, vehicles)}
	for i := range f.ids {
		f.ids[i] = fmt.Sprintf("EV-%05d", i)
	}
	return f
}

// event returns a fully populated sample, so payload size matches real telemetry.
func (f *fleet) event() *telemetryv1.TelemetryEvent {
	v := f.next.Add(1) % vehicles
	seq := f.seqs[v].Add(1)
	return &telemetryv1.TelemetryEvent{
		EventId: fmt.Sprintf("%s-%d", f.ids[v], seq), VehicleId: f.ids[v], Seq: seq,
		TimestampMs: time.Now().UnixMilli(), Vin: "7PDSGABA8PN000001", Model: "R1S", PackCapacityKwh: 135,
		SoftwareVersion: "2026.10.1", Region: "ca-west", SocPct: 71.3, BatteryTempC: 31.2, MotorTempC: 54.8,
		CoolantTempC: 27.4, AmbientTempC: 14.1, CoolingDemandPct: 22.5, SpeedKph: 87.6, Latitude: 49.2827,
		Longitude: -123.1207, HeadingDeg: 182.4, OdometerKm: 18234.7,
		ChargingState: telemetryv1.ChargingState_CHARGING_STATE_NONE, PackPowerKw: 31.9,
		EnergyConsumedKwh: 4120.2, EnergyChargedKwh: 4188.9,
		Connectivity: telemetryv1.ConnectivityState_CONNECTIVITY_STATE_ONLINE, SignalDbm: -84,
	}
}

func (f *fleet) batch(n int) *telemetryv1.TelemetryBatch {
	b := &telemetryv1.TelemetryBatch{Events: make([]*telemetryv1.TelemetryEvent, n)}
	for i := range b.Events {
		b.Events[i] = f.event()
	}
	return b
}

func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return float64(sorted[min(int(float64(len(sorted))*p), len(sorted)-1)].Microseconds()) / 1000
}

// clearStreams removes only the telemetry streams, so a run starts empty without
// touching anything else in the Redis instance it is pointed at.
func clearStreams(ctx context.Context, rdb *redis.Client, shards int) error {
	for shard := range shards {
		if err := rdb.Unlink(ctx, stream.Key(shard)).Err(); err != nil {
			return err
		}
	}
	return nil
}

func countStored(ctx context.Context, rdb *redis.Client, shards int) (int64, error) {
	var stored int64
	for shard := range shards {
		cursor := "-"
		for {
			msgs, err := rdb.XRangeN(ctx, stream.Key(shard), cursor, "+", 500).Result()
			if err != nil {
				return 0, err
			}
			if len(msgs) == 0 {
				break
			}
			for _, m := range msgs {
				var b telemetryv1.TelemetryBatch
				if err := proto.Unmarshal([]byte(m.Values[stream.PayloadField].(string)), &b); err != nil {
					return 0, err
				}
				stored += int64(len(b.Events))
			}
			cursor = "(" + msgs[len(msgs)-1].ID
		}
	}
	return stored, nil
}

func main() {
	mode := flag.String("mode", "open", "open: fixed arrival rate; closed: callers send as fast as acknowledged")
	batch := flag.Int("batch", 1, "events per request")
	rate := flag.Int("rate", 10000, "open: target events per second")
	conc := flag.Int("conc", 64, "closed: concurrent callers")
	dur := flag.Duration("dur", 15*time.Second, "measured duration")
	warm := flag.Duration("warm", 3*time.Second, "warm-up, not measured")
	target := flag.String("grpc", "127.0.0.1:9090", "ingestion gRPC address")
	redisAddr := flag.String("redis", "127.0.0.1:6379", "Redis address the service writes to")
	shards := flag.Int("shards", 16, "must match the service's STREAM_SHARDS")
	flag.Parse()

	ctx := context.Background()
	// Reading back or unlinking a large run can take far longer than a normal command.
	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr, ReadTimeout: 2 * time.Minute})
	defer rdb.Close()
	if err := clearStreams(ctx, rdb, *shards); err != nil {
		log.Fatalf("clear streams: %v", err)
	}

	clients := make([]telemetryv1.TelemetryServiceClient, connections)
	for i := range clients {
		c, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("grpc client: %v", err)
		}
		defer c.Close()
		clients[i] = telemetryv1.NewTelemetryServiceClient(c)
	}
	authed := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+os.Getenv("INGEST_API_KEY"))

	f := newFleet()
	var (
		acked, errs, measured atomic.Int64
		nextClient            atomic.Uint64
		mu                    sync.Mutex
		latencies             []time.Duration
		wg                    sync.WaitGroup
	)
	start := time.Now()
	measureFrom, stopAt := start.Add(*warm), start.Add(*warm+*dur)

	send := func(intended time.Time) {
		reqCtx, cancel := context.WithTimeout(authed, 5*time.Second)
		defer cancel()
		resp, err := clients[nextClient.Add(1)%connections].IngestBatch(reqCtx, f.batch(*batch))
		latency := time.Since(intended)
		if err != nil || len(resp.GetRejected()) > 0 {
			if errs.Add(1) == 1 {
				log.Printf("first failed request: err=%v rejected=%d", err, len(resp.GetRejected()))
			}
			return
		}
		acked.Add(int64(resp.GetAccepted()))
		if !intended.Before(measureFrom) {
			measured.Add(int64(resp.GetAccepted()))
			mu.Lock()
			latencies = append(latencies, latency)
			mu.Unlock()
		}
	}

	label := fmt.Sprintf("open rate=%d", *rate)
	if *mode == "closed" {
		label = fmt.Sprintf("closed conc=%d", *conc)
		for range *conc {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for now := time.Now(); !now.After(stopAt); now = time.Now() {
					send(now)
				}
			}()
		}
	} else {
		interval := time.Duration(float64(time.Second) * float64(*batch) / float64(*rate))
		for i := 0; ; i++ {
			at := start.Add(time.Duration(i) * interval)
			if at.After(stopAt) {
				break
			}
			if d := time.Until(at); d > 0 {
				time.Sleep(d)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				send(at)
			}()
		}
	}
	wg.Wait()

	stored, err := countStored(ctx, rdb, *shards)
	if err != nil {
		log.Fatalf("count stored events: %v", err)
	}
	if err := clearStreams(ctx, rdb, *shards); err != nil {
		log.Fatalf("clear streams: %v", err)
	}

	slices.Sort(latencies)
	fmt.Printf("%-18s batch=%-4d achieved=%8.0f ev/s  p50=%7.2fms p99=%7.2fms p99.9=%7.2fms max=%7.2fms  errs=%d acked=%d stored=%d lost=%d\n",
		label, *batch, float64(measured.Load())/dur.Seconds(),
		percentile(latencies, .5), percentile(latencies, .99), percentile(latencies, .999), percentile(latencies, 1),
		errs.Load(), acked.Load(), stored, max(0, acked.Load()-stored))
	if errs.Load() > 0 || acked.Load() > stored {
		os.Exit(1)
	}
}
