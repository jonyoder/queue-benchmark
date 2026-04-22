# Session Handoff — queue-benchmark

This file is a self-contained context dump for forking a new Claude Code session on this benchmark project. If you're starting fresh, read this first.

## What is this repo

A vendor-neutral benchmark harness for Postgres-backed Go job queues. Currently compares [River](https://github.com/riverqueue/river) and [rstudio/platform-lib](https://github.com/rstudio/platform-lib)'s rsqueue. Full positioning is in [`README.md`](README.md).

**Intentional neutrality:** the repo is public and framed as a generic multi-tenant-SaaS benchmark. **Do not reference Keavi** (the product that motivated this benchmark) or any financial-app terminology in commits, code, or docs — the patterns are generic enough to be broadly interesting.

## What state it's in

All three phases shipped, plus a variance pass and a follow-up round. 54 benchmark runs across 18 scenario × library pairs. Final numbers live in [`REPORT.md`](REPORT.md) at the repo root.

### Completed
- **Phase 1** (`9b84909`): harness bootstrap, `Queue` interface, JSONL recorder, docker-compose Postgres, skeleton CLI.
- **Phase 2** (`8250b2f`, `fd189dd`): River adapter (~300 lines) + platform-lib adapter (~1,080 lines). Platform-lib required a hand-written `QueueStore` because no production Postgres impl ships in the library.
- **Phase 3A** (`1189fc0`): runner + workload generator + 5 scenarios + CLI.
- **Phase 3B + 4** (`e1273f6`): JSONL analyzer (`bench report`), `scripts/run-all.sh`, first `REPORT.md`.
- **Variance pass** (`33894cf`): N=3 per cell, warmup filter, median+spread, 7 scenarios.
- **Follow-up round** (`c893e49`, `99fc190`): exponential backoff in platform-lib adapter, `noisy_neighbor_saturated` scenario, `high_scale` scenario.

### Deferred with design notes

[`docs/FUTURE.md`](docs/FUTURE.md) has concrete plans for:
1. `crash_recovery` scenario (parent/child process split for SIGKILL injection) — ~2–4h.
2. `rscache` + `AddressedPush` integration — ~2–4h.
3. Jitter in `ExponentialBackoffRiverLike` — ~30 min.
4. Multi-process scenario — ~1h.
5. DB-backed backoff in platform-lib adapter (durable retry, matches River's model) — ~1–2h.

## Key design decisions locked

- **Adapter parity:** both libraries expose the same `Harness` interface (`internal/queue/queue.go`). Dispatching is by string `Kind`; a single bench-wide `JobArgs`/work type carries the kind for library APIs that want compile-time typing.
- **Generic job taxonomy:** `document_process`, `entity_update`, `entity_enrich`, `tenant_rollup`, `tenant_snapshot`, `daily_coordinator`, `monthly_coordinator`, `notification_deliver`. **No financial/product-specific names.**
- **Metrics format:** newline-delimited JSON, one file per run, correlated by `job_id`. Schema in `internal/metrics/recorder.go`.
- **Warmup:** 2-second warmup window excluded from latency statistics.
- **Retry:** platform-lib adapter implements retry via goroutine-based `time.AfterFunc` — a deliberate choice that trades durability for responsiveness. Optional `BackoffFn` matches River's `attempt^4` backoff for apples-to-apples.

## Top findings

1. **LISTEN/NOTIFY responsiveness**: platform-lib pickup p95 is **16–63× faster** than River under-capacity. Gap widens with rate (63× at 300 Hz, 18× at sparse rate). Reproducible, tight run-to-run variance.
2. **Saturated throughput**: platform-lib completes ~25–30% more jobs/sec than River under backlog. Sensitive to adapter-implementation choices; directional rather than absolute.
3. **Retry shape**: with matched backoff, both libraries produce identical retry counts. Pickup p95 differs ~1000× due to goroutine-vs-DB scheduling — a durability-vs-responsiveness tradeoff, not a correctness gap.
4. **Fairness**: natural FIFO fairness in both libraries; neither has per-tenant priority.
5. **Resource usage**: River runs ~2× more goroutines (leader-election, periodic workers). RSS roughly similar.

## Honest caveats from the final report

- **One hardware datapoint** (Apple Silicon, Postgres 16 in docker-compose). Real deployments may differ.
- **Synthetic workload** — `time.Sleep` to model work, not real CPU/IO.
- **Adapter implementation sensitivity** — particularly the platform-lib `QueueStore` uses `FOR UPDATE SKIP LOCKED` with a fast-path idle count; a different implementation could shift throughput numbers.
- **Only two of five follow-up items implemented; three deferred.**

## How to pick up benchmark work

### Resume environment

```bash
cd /Users/jonyoder/Dev/queue-benchmark
make up                                # start Postgres
QB_POSTGRES_URL='postgres://benchmark:benchmark@localhost:5433/benchmark?sslmode=disable' \
    go test -p 1 ./...                 # verify everything still passes
```

### Re-generate report from existing raw data

```bash
./bin/bench report --results-dir=./results --out=./results/REPORT.md
```

### Run a single scenario

```bash
./bin/bench run \
    --lib=river \                      # or platlib
    --scenario=steady_under \          # see --help for list
    --postgres-url=$QB_POSTGRES_URL \
    --results-dir=./results \
    --duration=30s \
    --workers=20
```

### Re-run the full sweep

```bash
# ~30 min wall-clock, 42 runs
DURATION=30s RUNS=3 ./scripts/run-all.sh

# Separate follow-up scenarios (items 2+3)
./scripts/run-items-2-3.sh
```

## What to work on next (suggested priority)

- **Highest leverage:** DB-backed backoff in the platform-lib adapter. Removes the "retry durability asymmetry" caveat and makes `rate_limit_pressure` comparable across *implementation strategies*, not just outcomes. [`docs/FUTURE.md`](docs/FUTURE.md) has the design.
- **Most novel finding potential:** `crash_recovery`. Neither library has been tested under process-kill mid-run in this benchmark. Might surface real durability differences. Parent/child process split needed.
- **Biggest "completeness" win:** `rscache` + `AddressedPush` integration. Benchmarks platform-lib's genuinely-unique architectural feature. Not head-to-head with River (River has no equivalent) — but a capability benchmark.
- **Easy and clarifying:** add jitter to `ExponentialBackoffRiverLike`. River uses ±10%; without jitter, my backoff has artificially-tight pickup-p95 variance. ~30 min.

## Filesystem layout

```
/Users/jonyoder/Dev/queue-benchmark/
├── README.md                          # public-facing positioning
├── REPORT.md                          # current benchmark results + interpretation
├── HANDOFF.md                         # this file
├── LICENSE                            # MIT
├── Makefile                           # up/down/build/test/bench/report
├── docker-compose.yml                 # ephemeral Postgres 16 on port 5433
├── go.mod                             # go 1.26, deps: pgx, river, platform-lib, uuid
├── cmd/bench/main.go                  # CLI: `bench run` + `bench report`
├── internal/
│   ├── queue/
│   │   ├── queue.go                   # vendor-neutral Harness interface
│   │   ├── harness.go                 # Config, Simulate helper, error classes
│   │   ├── testhelp/pg.go             # pgxpool test helpers
│   │   ├── river/adapter.go           # River implementation (~300 lines)
│   │   └── platlib/                   # platform-lib implementation (~1,080 lines)
│   │       ├── adapter.go             # Harness wiring + agent loop
│   │       ├── store.go               # QueueStore over pgx (~440 lines)
│   │       ├── schema.sql             # Postgres DDL
│   │       ├── constants.go           # notify type / channel constants
│   │       └── adapter_test.go        # integration tests
│   ├── workload/
│   │   ├── workload.go                # Spec, Generator, RateCurve
│   │   └── scenarios.go               # 9 scenarios: steady_*, burst, noisy_*, rate_*, notify_*, high_scale
│   ├── runner/runner.go               # Orchestration: generator → harness → recorder
│   ├── metrics/recorder.go            # JSONL recorder
│   └── analyze/
│       ├── analyze.go                 # Reads JSONL, computes stats, renders Markdown
│       └── analyze_test.go            # Percentile / warmup / aggregation tests
├── docs/
│   ├── METHODOLOGY.md                 # Workload model, metrics, reproducibility
│   └── FUTURE.md                      # Deferred work with design notes
├── scripts/
│   ├── run-all.sh                     # Full sweep: 7 scenarios × 2 libs × 3 runs
│   └── run-items-2-3.sh               # Follow-up scenarios only
└── results/                           # .gitignored; reproducible via scripts
```

## Anything weird I should know

- **Go 1.26 required** (platform-lib v3 requires it). `go mod tidy` auto-downloads the toolchain.
- **macOS bash 3.2 compat**: scripts avoid associative arrays. Use `case` blocks inside functions for per-scenario config.
- **Cross-package test parallelism**: `go test ./...` runs package-level tests in parallel, but both adapters share the same Postgres DB and truncate its schema. **Always run tests with `-p 1`** (the Makefile does this by default in `make test`).
- **Docker pitfall**: `lsof -ti:PORT` returns the docker backend proxy PID too. Don't kill blindly.

## The original motivation (private to Keavi context)

This benchmark was spawned from Keavi's scalability audit to answer: "should we switch from River to platform-lib?" The Keavi-side context is in Keavi's memory at `memory/project_queue_benchmark_sprint.md`. The answer — per the final REPORT — is *stay on River for queuing; adopt platform-lib's cache module independently when LLM-response caching becomes a cost lever*.

This file is **intentionally silent** on that motivation because the benchmark results are broadly useful to anyone evaluating these libraries — tying them to one product narrows the audience.
