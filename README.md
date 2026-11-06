# EV Telemetry Intelligence Platform

A connected-vehicle telemetry platform that explains why a vehicle is behaving abnormally, not just what its sensors read.

**Status:** in progress. The backend pipeline is built: simulator, telemetry ingestion, anomaly engine and alert service. The dashboard and deployment setup are still to come.

## Run it locally

Requires Go, Redis and PostgreSQL 16 (`brew install go redis postgresql@16`). No Docker needed.

```bash
scripts/local.sh up 300     # start the backend with 300 simulated vehicles
python3 scripts/score_live.py   # compare live alerts with the injected faults
scripts/local.sh down       # stop everything and delete local data
```

## Run the tests

```bash
scripts/local.sh test
```
