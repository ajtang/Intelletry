# Ingestion benchmarks

These results measure how fast `telemetry-ingestion` accepts telemetry over gRPC and writes it to Redis. They don't measure the full pipeline: the anomaly engine and alert service weren't running.

To reproduce them:

```bash
scripts/bench.sh quick   # one short run, to check that everything works
scripts/bench.sh         # three runs at each level, about 5 minutes and up to 5 GB of Redis memory
```

## Results

Measured at commit `cac92d2`. Each level ran six times, in two sets of three, with 15 measured seconds per run after a 3-second warm-up. The first set used an earlier draft of the load generator that cleared Redis differently between runs. The events were 235–238 bytes each, across 10,000 simulated vehicles.

| Load | Batch size | p50 | p99, typical | p99, worst | Failed requests | Lost events |
|---|---:|---:|---:|---:|---:|---:|
| 20,000 events/s | 1 | 5.6 ms | 10.6 ms | 10.6 ms | 0 | 0 |
| 500,000 events/s | 100 | 6.2 ms | 10.9 ms | 19.2 ms | 0 | 0 |
| 1,000,000 events/s | 100 | 6.3–7.0 ms | 11.9 ms | 454 ms | 431 in one run | 0 |

- **500,000 events/s is the highest level that held up.** All six runs stayed under 20 ms p99 with no failures.
- **1,000,000 events/s isn't stable on this setup.** Four runs stayed under 19 ms p99 and two reached 195 ms and 454 ms. The 454 ms run also had 431 requests refused with `Unavailable`. The cause hasn't been diagnosed, and the machine was also running other work.
- **The latency floor is mostly batching.** Requests wait up to `BATCH_LINGER` (10 ms) to share a Redis pipeline, so a p99 near 11 ms is close to the lowest this configuration can reach.
- **The ceiling wasn't found in a controlled way.** One closed-loop run with 256 callers reached 2.37 million events/s at 25 ms p99, but it wasn't repeated and shouldn't be quoted.

## How it's measured

- **Latency:** runs use a fixed arrival rate and time each request from when it should have been sent, so a stalled server can't hide behind a stalled client.
- **Lost events:** after each run, the shard streams are read back and counted. An event counts as lost if it was acknowledged but isn't in Redis.

## Limits of these numbers

- **One machine:** the load generator, service and Redis shared an Apple M4 Pro over localhost, with a load average of about 9 from other work. Real networks and TLS would add latency.
- **No persistence:** Redis ran without RDB or AOF, as in `scripts/local.sh`. An acknowledgement means the event is in Redis memory, so a Redis crash would lose it.
- **Rate limit raised:** the default of 20,000 events/s per credential was lifted so that it wouldn't cap the measurement. At the default, sustained load above that is refused.
- **Short runs:** 15 seconds doesn't show behaviour under stream trimming, consumer lag, or sustained memory pressure.
