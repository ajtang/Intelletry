#!/usr/bin/env bash
# Measures telemetry-ingestion against a throwaway Redis on private ports. Requires go
# and redis. Results and their limits are explained in docs/benchmarks.md.
#
#   scripts/bench.sh          three runs at each load level, about 5 minutes and up to 5 GB of Redis memory
#   scripts/bench.sh quick    one short run at a low rate, to check that everything works
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REDIS_PORT=16399
GRPC_PORT=19090
HTTP_PORT=18080

for port in "$REDIS_PORT" "$GRPC_PORT" "$HTTP_PORT"; do
  if lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "port $port is already in use" >&2
    exit 1
  fi
done

WORK="$(mktemp -d)"
PIDS=()
cleanup() {
  for pid in ${PIDS[@]+"${PIDS[@]}"}; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

(cd "$ROOT" && go build -o "$WORK/ingest" ./services/telemetry-ingestion && go build -o "$WORK/loadgen" ./scripts/loadgen)

# Local-only value for the throwaway service.
export INGEST_API_KEY="local-bench-ingest-key-0001"

# Persistence is off to match scripts/local.sh. The rate limit is raised so that it does
# not cap the measurement; its default is 20,000 events per second per credential.
redis-server --port "$REDIS_PORT" --bind 127.0.0.1 --dir "$WORK" --save "" --appendonly no --logfile "$WORK/redis.log" &
PIDS+=($!)
sleep 1
env REDIS_URL="redis://127.0.0.1:$REDIS_PORT/0" INGEST_API_KEYS="bench:$INGEST_API_KEY" \
  HTTP_PORT="$HTTP_PORT" GRPC_PORT="$GRPC_PORT" SHUTDOWN_GRACE=0s \
  RATE_LIMIT_EPS=1000000000 RATE_LIMIT_BURST=1000000000 "$WORK/ingest" >"$WORK/ingest.log" 2>&1 &
PIDS+=($!)
sleep 1

load() { "$WORK/loadgen" -grpc "127.0.0.1:$GRPC_PORT" -redis "127.0.0.1:$REDIS_PORT" "$@"; }

echo "commit $(git -C "$ROOT" rev-parse --short HEAD), $(sysctl -n machdep.cpu.brand_string 2>/dev/null || uname -m), $(uptime | sed 's/.*load/load/')"
if [ "${1:-}" = "quick" ]; then
  load -mode open -batch 100 -rate 20000 -dur 3s -warm 1s
  exit 0
fi
# A failed run is a result, so the rest still run and the script reports it at the end.
status=0
for _ in 1 2 3; do
  load -mode open -batch 1 -rate 20000 || status=1
  load -mode open -batch 100 -rate 500000 || status=1
  load -mode open -batch 100 -rate 1000000 || status=1
done
echo "finished, $(uptime | sed 's/.*load/load/')"
exit "$status"
