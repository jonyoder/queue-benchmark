#!/usr/bin/env bash
# run-items-2-3.sh — runs the follow-up scenarios added for items 2 and 3
# of the autonomous sprint: noisy_neighbor_saturated (fairness under
# pressure) and high_scale (higher absolute rates with low utilization).
set -euo pipefail

RUNS=${RUNS:-3}
PG_URL=${PG_URL:-postgres://benchmark:benchmark@localhost:5433/benchmark?sslmode=disable}
RESULTS=${RESULTS:-./results}
BENCH=${BENCH:-./bin/bench}

reset_schema() {
    docker compose exec -T postgres \
        psql -U benchmark -d benchmark -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' \
        > /dev/null 2>&1
}

run_cell() {
    local lib="$1" scenario="$2" iter="$3" workers="$4" duration="$5"
    reset_schema
    echo "== $lib / $scenario (run $iter/$RUNS, workers=$workers, duration=$duration) =="
    "$BENCH" run --lib="$lib" --scenario="$scenario" \
        --postgres-url="$PG_URL" --results-dir="$RESULTS" \
        --duration="$duration" --workers="$workers"
}

# Saturated fairness: 100 Hz, 10 workers (saturated at ~100 jobs/sec drain)
# with 90% tenant skew. Real starvation pressure at the queue tail.
for lib in river platlib; do
    for i in $(seq 1 "$RUNS"); do
        run_cell "$lib" "noisy_neighbor_saturated" "$i" 10 30s
    done
done

# High scale: 300 Hz against 100 workers — well under capacity but at a
# much higher absolute rate than prior scenarios. Exercises Postgres
# coordination overhead at rate.
for lib in river platlib; do
    for i in $(seq 1 "$RUNS"); do
        run_cell "$lib" "high_scale" "$i" 100 15s
    done
done

echo "Done. Regenerating report..."
"$BENCH" report --results-dir="$RESULTS" --out="$RESULTS/REPORT.md"
echo "Report at $RESULTS/REPORT.md"
