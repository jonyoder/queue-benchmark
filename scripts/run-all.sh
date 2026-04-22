#!/usr/bin/env bash
# run-all.sh — runs every scenario against every library and produces a
# fresh JSONL result file per (lib, scenario) combo. After the run the
# `bench report` command aggregates them into REPORT.md.
#
# Usage:
#   scripts/run-all.sh               # 30s per scenario at 100 Hz
#   DURATION=60s RATE=200 scripts/run-all.sh  # tuning knobs via env
set -euo pipefail

DURATION=${DURATION:-30s}
RATE=${RATE:-100}
PG_URL=${PG_URL:-postgres://benchmark:benchmark@localhost:5433/benchmark?sslmode=disable}
RESULTS=${RESULTS:-./results}
BENCH=${BENCH:-./bin/bench}

if ! docker compose ps --status running | grep -q queue-benchmark-postgres; then
    echo "Postgres is not running. Start it with 'make up'."
    exit 1
fi

if [[ ! -x "$BENCH" ]]; then
    echo "Building $BENCH..."
    go build -o "$BENCH" ./cmd/bench
fi

mkdir -p "$RESULTS"
# Clear any existing JSONL so the report is computed from this batch only.
rm -f "$RESULTS"/*.jsonl

reset_schema() {
    docker compose exec -T postgres \
        psql -U benchmark -d benchmark -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' \
        > /dev/null 2>&1
}

run() {
    local lib="$1"
    local scenario="$2"
    reset_schema
    echo "== $lib / $scenario =="
    "$BENCH" run \
        --lib="$lib" \
        --scenario="$scenario" \
        --postgres-url="$PG_URL" \
        --results-dir="$RESULTS" \
        --duration="$DURATION" \
        --rate="$RATE"
}

SCENARIOS=(steady burst noisy_neighbor rate_limit_pressure notify_latency)
LIBS=(river platlib)

for lib in "${LIBS[@]}"; do
    for scn in "${SCENARIOS[@]}"; do
        run "$lib" "$scn"
    done
done

echo
echo "All runs complete. Generating report..."
"$BENCH" report --results-dir="$RESULTS" --out="$RESULTS/REPORT.md"
echo "Done. Report at $RESULTS/REPORT.md"
