#!/usr/bin/env bash
# Run the gateway benchmark scenarios end to end on this machine:
#   kelvran-bench upstream (mock provider)  <-  gateway  <-  kelvran-bench run
# One gateway process per scenario (fresh cache and state each time), a
# generated config and a random virtual key held in a 0600 file, nothing
# listening beyond loopback. Writes results.json, bench.json (github-action-
# benchmark customSmallerIsBetter) and summary.md into $BENCH_OUT.
#
# Environment (all optional):
#   BENCH_OUT         output directory (default ./bench-out)
#   BENCH_SCENARIOS   space-separated preset names (default "S1a S1b S2 S3";
#                     S4 is added automatically when BENCH_REDIS_ADDR is set)
#   BENCH_RPS         offered rate override (default: preset; S6 keeps its own steps and says so)
#   BENCH_DURATION    measured window override, e.g. 30s (default: preset)
#   BENCH_WARMUP      warm-up override, e.g. 5s (default: preset)
#   BENCH_REDIS_ADDR  host:port of a Redis; adds S4 (two replicas sharing it). Only S4's
#                     gateways are configured with Redis — every other scenario runs the
#                     gateway with its in-memory stores, so S1a-S3 measure the gateway alone
#   BENCH_BASELINE    when set, also runs each scenario straight at the mock (no gateway),
#                     recorded as <scenario>-baseline (S6-baseline-<rate>rps for the sweep): the
#                     harness's and the mock's own floor,
#                     which the gateway numbers must be read against
#   BENCH_PORT_BASE   first loopback port to use (default 9400; the 9000s collide with common local daemons)
#   BENCH_KEEP_WORK   when set, keeps the temporary work directory (configs, logs) for debugging
# Numbers from a laptop or a hosted runner are trends, never release numbers:
# docs/operations/BENCHMARKS.md.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${BENCH_OUT:-$ROOT/bench-out}
SCENARIOS=${BENCH_SCENARIOS:-"S1a S1b S2 S3"}
RPS=${BENCH_RPS:-}
DURATION=${BENCH_DURATION:-}
WARMUP=${BENCH_WARMUP:-}
REDIS=${BENCH_REDIS_ADDR:-}
PORT=${BENCH_PORT_BASE:-9400}
WORK=$(mktemp -d)
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null || true
  done
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && wait "$pid" 2>/dev/null || true
  done
  if [ -z "${BENCH_KEEP_WORK:-}" ]; then rm -rf "$WORK"; else echo "bench-ci: kept $WORK (BENCH_KEEP_WORK set)"; fi
}
trap cleanup EXIT

mkdir -p "$OUT"
rm -f "$OUT/results.json" "$OUT/bench.json" "$OUT/summary.md" "$OUT/gateway.log" "$OUT/upstream.log"

echo "bench-ci: building gateway and kelvran-bench"
(cd "$ROOT/gateway" && go build -o "$WORK/gateway" ./cmd/gateway && go build -o "$WORK/kelvran-bench" ./cmd/kelvran-bench)
GATEWAY_VERSION=$("$WORK/gateway" -version)
echo "bench-ci: $GATEWAY_VERSION"

# The virtual-key secret lives only in a 0600 file; the gateway sees its hash.
old_umask=$(umask)
umask 077
SECRET=$(python3 -c 'import secrets; print(secrets.token_hex(32))')
printf '%s\n' "$SECRET" > "$WORK/key"
KEY_HASH=$(printf '%s' "$SECRET" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')
unset SECRET
umask "$old_umask"
export KELVRAN_BENCH_UPSTREAM_KEY="bench-upstream-key"
export KELVRAN_BENCH_PROPAGATION_SECRET
KELVRAN_BENCH_PROPAGATION_SECRET=$(python3 -c 'import secrets; print(secrets.token_hex(32))')

wait_http() { # url, seconds
  for _ in $(seq 1 "$2"); do curl -fsS "$1" >/dev/null 2>&1 && return 0; sleep 1; done
  echo "bench-ci: $1 not ready after $2 s" >&2; return 1
}

write_config() { # path listen_port upstream_port [redis]  — the Redis block only when the 4th arg is "redis" (S4)
  cat > "$1" <<YAML
listen_addr: "127.0.0.1:$2"
virtual_keys:
  bench:
    key_hash: "$KEY_HASH"
    rate_limit:
      burst: 100000
      refill_per_second: 100000
      max_concurrent_requests: 10000
deployments:
  bench:
    model: "bench"
    provider: "openai"
    upstream_model: "bench"
    base_url: "http://127.0.0.1:$3/v1/chat/completions"
    api_key_env: "KELVRAN_BENCH_UPSTREAM_KEY"
    allow_insecure_http: true
telemetry:
  exporter: "none"
YAML
  if [ "${4:-}" = "redis" ]; then
    cat >> "$1" <<YAML
rate_limit:
  redis_addr: "$REDIS"
budget:
  redis_addr: "$REDIS"
admin:
  redis_addr: "$REDIS"
config_propagation:
  redis_addr: "$REDIS"
  signing_secret_env: "KELVRAN_BENCH_PROPAGATION_SECRET"
YAML
  fi
}

start_upstream() { # scenario port
  "$WORK/kelvran-bench" upstream -scenario "$1" -listen "127.0.0.1:$2" >> "$OUT/upstream.log" 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 1 20); do (echo > "/dev/tcp/127.0.0.1/$2") >/dev/null 2>&1 && return 0; sleep 0.25; done
  echo "bench-ci: upstream on $2 did not come up" >&2; return 1
}

start_gateway() { # config listen_port -> sets GW_PID (no subshell: the pid must land in PIDS so cleanup can stop it)
  "$WORK/gateway" -config "$1" >> "$OUT/gateway.log" 2>&1 &
  GW_PID=$!
  PIDS+=("$GW_PID")
  wait_http "http://127.0.0.1:$2/healthz" 30
}

stop_pids() {
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null || true; done
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && wait "$pid" 2>/dev/null || true; done
  PIDS=()
}

run_args=()
[ -n "$RPS" ] && run_args+=(-rps "$RPS")
[ -n "$DURATION" ] && run_args+=(-duration "$DURATION")
[ -n "$WARMUP" ] && run_args+=(-warmup "$WARMUP")

if [ -n "$REDIS" ] && ! grep -qw S4 <<<"$SCENARIOS"; then SCENARIOS="$SCENARIOS S4"; fi

port_free() { ! (echo > "/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1; }

for S in $SCENARIOS; do
  UP_PORT=$((PORT + 1)); GW_PORT=$((PORT + 2)); GW2_PORT=$((PORT + 3))
  for p in "$UP_PORT" "$GW_PORT" "$GW2_PORT"; do
    port_free "$p" || { echo "bench-ci: port $p is already in use; set BENCH_PORT_BASE or stop the listener" >&2; exit 1; }
  done
  echo "bench-ci: === $S ==="
  REDIS_FLAG=""
  if [ "$S" = "S4" ]; then
    if [ -z "$REDIS" ]; then echo "bench-ci: S4 needs BENCH_REDIS_ADDR; skipping" >&2; continue; fi
    REDIS_FLAG=redis
  fi
  start_upstream "$S" "$UP_PORT"
  write_config "$WORK/config-$S.yaml" "$GW_PORT" "$UP_PORT" "$REDIS_FLAG"
  "$WORK/gateway" -config "$WORK/config-$S.yaml" -validate >/dev/null
  start_gateway "$WORK/config-$S.yaml" "$GW_PORT"
  FIRST_GW_PID=$GW_PID
  TARGETS="http://127.0.0.1:$GW_PORT"
  if [ "$S" = "S4" ]; then
    write_config "$WORK/config-$S-2.yaml" "$GW2_PORT" "$UP_PORT" "$REDIS_FLAG"
    start_gateway "$WORK/config-$S-2.yaml" "$GW2_PORT"
    TARGETS="$TARGETS,http://127.0.0.1:$GW2_PORT"
  fi
  COMMON_META=(-meta "gateway_version=$GATEWAY_VERSION" -meta "host=$(uname -srm)" -meta "cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo unknown)")
  # ${arr[@]+"${arr[@]}"} expands to nothing for an empty array under set -u (bash 3.2, macOS).
  "$WORK/kelvran-bench" run -scenario "$S" -targets "$TARGETS" -key-file "$WORK/key" -model bench \
    -gateway-pid "$FIRST_GW_PID" -out "$OUT/results.json" -bench-json "$OUT/bench.json" -summary "$OUT/summary.md" \
    "${COMMON_META[@]}" -meta "redis=$([ -n "$REDIS_FLAG" ] && echo "$REDIS" || echo none)" ${run_args[@]+"${run_args[@]}"}
  if [ -n "${BENCH_BASELINE:-}" ]; then
    # The same load straight at the mock: what the harness and the mock cost on their own.
    "$WORK/kelvran-bench" run -scenario "$S" -name "$S-baseline" -targets "http://127.0.0.1:$UP_PORT" -key-file "$WORK/key" -model bench \
      -out "$OUT/results.json" -bench-json "$OUT/bench.json" -summary "$OUT/summary.md" \
      "${COMMON_META[@]}" -meta "redis=none" -meta "baseline=true" ${run_args[@]+"${run_args[@]}"}
  fi
  stop_pids
  PORT=$((PORT + 10))
done

if [ ! -f "$OUT/summary.md" ]; then echo "bench-ci: no scenario ran (S4 needs BENCH_REDIS_ADDR)" >&2; exit 1; fi
echo "bench-ci: done — $OUT/results.json, $OUT/bench.json, $OUT/summary.md"
cat "$OUT/summary.md"
