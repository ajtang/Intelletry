#!/usr/bin/env bash
# Runs the whole backend on this machine without Docker, against throwaway Redis and
# Postgres instances kept in .run/. Requires: brew install go redis postgresql@16
#
#   scripts/local.sh up [vehicles]     start everything (default 200 vehicles)
#   scripts/local.sh down              stop everything and delete .run/
#   scripts/local.sh status            show what is running
#   scripts/local.sh test              run all tests against throwaway databases
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN="$ROOT/.run"
PG_BIN="${PG_BIN:-/opt/homebrew/opt/postgresql@16/bin}"
REDIS_PORT=16379
PG_PORT=15432
export LC_ALL=en_US.UTF-8

# Local-only development values. Real deployments take these from Azure Key Vault.
export REDIS_URL="redis://127.0.0.1:$REDIS_PORT/0"
export DATABASE_URL="postgres://dev@127.0.0.1:$PG_PORT/fleet?sslmode=disable"
export INGEST_KEY="local-dev-ingest-key-0001"
export API_TOKEN="local-dev-api-token-0001"

infra_up() {
  mkdir -p "$RUN/redis" "$RUN/logs" "$RUN/bin" "$RUN/pids"
  if [ ! -d "$RUN/pg" ]; then
    "$PG_BIN/initdb" -D "$RUN/pg" -U dev --auth=trust -E UTF8 >/dev/null
  fi
  if ! "$PG_BIN/pg_ctl" -D "$RUN/pg" status >/dev/null 2>&1; then
    "$PG_BIN/pg_ctl" -D "$RUN/pg" -o "-p $PG_PORT -c unix_socket_directories='' -c listen_addresses=127.0.0.1" -l "$RUN/logs/postgres.log" -w start >/dev/null
    "$PG_BIN/createdb" -h 127.0.0.1 -p "$PG_PORT" -U dev fleet 2>/dev/null || true
  fi
  if ! redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1; then
    redis-server --port "$REDIS_PORT" --dir "$RUN/redis" --save "" --daemonize yes --logfile "$RUN/logs/redis.log" >/dev/null
  fi
}

start() {
  local name="$1"; shift
  env "$@" "$RUN/bin/$name" >>"$RUN/logs/$name${LOG_SUFFIX:-}.log" 2>&1 &
  echo $! >"$RUN/pids/$name${LOG_SUFFIX:-}.pid"
}

case "${1:-}" in
  up)
    VEHICLES="${2:-200}"
    infra_up
    (cd "$ROOT" && for s in vehicle-simulator telemetry-ingestion anomaly-engine alert-service; do go build -o "$RUN/bin/$s" "./services/$s"; done)
    start alert-service HTTP_PORT=8090 API_TOKEN="$API_TOKEN" SHUTDOWN_GRACE=0s
    start telemetry-ingestion HTTP_PORT=8080 GRPC_PORT=9090 INGEST_API_KEYS="sim:$INGEST_KEY" SHUTDOWN_GRACE=0s
    start anomaly-engine HTTP_PORT=8091 TELEMETRY_TIME_SCALE="${SIM_TIME_SCALE:-60}"
    sleep 1
    start vehicle-simulator HTTP_PORT=8081 INGEST_API_KEY="$INGEST_KEY" SIM_VEHICLES="$VEHICLES" SIM_TIME_SCALE="${SIM_TIME_SCALE:-60}" SIM_FAULT_RATE="${SIM_FAULT_RATE:-0.15}"
    echo "Backend running with $VEHICLES vehicles. API: http://localhost:8090 (token: $API_TOKEN). Logs: .run/logs"
    ;;
  down)
    for f in "$RUN"/pids/*.pid; do [ -e "$f" ] && kill "$(cat "$f")" 2>/dev/null || true; done
    sleep 2
    redis-cli -p "$REDIS_PORT" shutdown nosave 2>/dev/null || true
    "$PG_BIN/pg_ctl" -D "$RUN/pg" -m fast stop >/dev/null 2>&1 || true
    rm -rf "$RUN"
    echo "Stopped and cleaned up."
    ;;
  status)
    for f in "$RUN"/pids/*.pid; do
      [ -e "$f" ] || continue
      kill -0 "$(cat "$f")" 2>/dev/null && echo "up    $(basename "$f" .pid)" || echo "DOWN  $(basename "$f" .pid)"
    done
    ;;
  test)
    infra_up
    "$PG_BIN/createdb" -h 127.0.0.1 -p "$PG_PORT" -U dev evtest 2>/dev/null || true
    # -p 1 because these packages reset the same throwaway database.
    cd "$ROOT" && TEST_DATABASE_URL="postgres://dev@127.0.0.1:$PG_PORT/evtest?sslmode=disable" TEST_REDIS_URL="redis://127.0.0.1:$REDIS_PORT/1" \
      go test -race -count=1 -p 1 ./...
    ;;
  *)
    sed -n '2,8p' "$0"; exit 1 ;;
esac
