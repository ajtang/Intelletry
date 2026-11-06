# EV Telemetry Intelligence Platform

A connected-vehicle telemetry platform that explains why a vehicle is behaving abnormally, not just what its sensors read.

**Status:** in progress. The backend pipeline and fleet dashboard are built. The 3D digital twin and deployment setup are still to come.

## Run it locally

Requires Go, Redis, PostgreSQL 16 and Node 22+ (`brew install go redis postgresql@16 node`). No Docker needed.

```bash
scripts/local.sh up 300     # start the backend with 300 simulated vehicles
scripts/local.sh dashboard  # run the dashboard at http://localhost:3000
python3 scripts/score_live.py   # compare live alerts with the injected faults
scripts/local.sh down       # stop everything and delete local data
```

## Run the tests

```bash
scripts/local.sh test
```

## Benchmark ingestion

```bash
scripts/bench.sh
```

Ingestion sustained 500,000 events/s under 20 ms p99 with no lost events on one laptop. See [the results and their limits](docs/benchmarks.md).

## Dashboard access

The dashboard has no sign-in yet. The operator name on alert actions is typed in by the user and is not verified, so a real deployment needs Microsoft Entra ID or similar in front of it.
