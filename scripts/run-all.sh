#!/usr/bin/env bash
# run-all.sh — runs every scenario against every library N times per cell,
# with per-scenario worker counts, then aggregates results into REPORT.md.
#
# Usage:
#   scripts/run-all.sh
#   RUNS=5 DURATION=30s scripts/run-all.sh
set -euo pipefail

RUNS=${RUNS:-3}                # repetitions per (lib, scenario)
DURATION=${DURATION:-30s}      # run duration
PG_URL=${PG_URL:-postgres://benchmark:benchmark@localhost:5433/benchmark?sslmode=disable}
RESULTS=${RESULTS:-./results}
BENCH=${BENCH:-./bin/bench}

# Per-scenario worker count. Chosen to hit each scenario's target
# regime (under-capacity / balanced / saturated). Implemented as a
# case block (not associative array) for macOS bash 3.2 compatibility.
workers_for() {
    case "$1" in
        steady_under)           echo 20 ;; # under-capacity
        steady_balanced)        echo 20 ;; # ~40% utilization
        steady_over)            echo 10 ;; # saturated
        burst)                  echo 10 ;;
        noisy_neighbor)         echo 20 ;;
        rate_limit_pressure)    echo 10 ;;
        notify_latency)         echo 10 ;; # sparse
        *)                      echo 10 ;;
    esac
}

SCENARIOS=(
    steady_under
    steady_balanced
    steady_over
    burst
    noisy_neighbor
    rate_limit_pressure
    notify_latency
)
LIBS=(river platlib)

if ! docker compose ps --status running | grep -q queue-benchmark-postgres; then
    echo "Postgres is not running. Start it with 'make up'."
    exit 1
fi

if [[ ! -x "$BENCH" ]]; then
    echo "Building $BENCH..."
    go build -o "$BENCH" ./cmd/bench
fi

mkdir -p "$RESULTS"
rm -f "$RESULTS"/*.jsonl

reset_schema() {
    docker compose exec -T postgres \
        psql -U benchmark -d benchmark -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' \
        > /dev/null 2>&1
}

run() {
    local lib="$1" scenario="$2" iter="$3" workers="$4"
    reset_schema
    echo "== $lib / $scenario (run $iter/$RUNS, workers=$workers) =="
    "$BENCH" run \
        --lib="$lib" \
        --scenario="$scenario" \
        --postgres-url="$PG_URL" \
        --results-dir="$RESULTS" \
        --duration="$DURATION" \
        --workers="$workers"
}

total_runs=$(( ${#LIBS[@]} * ${#SCENARIOS[@]} * RUNS ))
echo "Queueing up $total_runs benchmark runs."
echo

run_n=0
start_ts=$(date +%s)
for lib in "${LIBS[@]}"; do
    for scn in "${SCENARIOS[@]}"; do
        workers=$(workers_for "$scn")
        for i in $(seq 1 "$RUNS"); do
            run_n=$((run_n + 1))
            echo "[$run_n/$total_runs]"
            run "$lib" "$scn" "$i" "$workers"
        done
    done
done

end_ts=$(date +%s)
echo
echo "All runs complete in $((end_ts - start_ts))s. Generating report..."
"$BENCH" report --results-dir="$RESULTS" --out="$RESULTS/REPORT.md"
echo "Done. Report at $RESULTS/REPORT.md"
