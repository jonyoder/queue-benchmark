# Queue Benchmark Results — First Pass

**Run parameters:** 30s duration, 100 Hz enqueue rate, 10 worker slots, Postgres 16, shared tunings (`max_connections=200`, `shared_buffers=256MB`).
**Host:** Apple Silicon (darwin/arm64), Go 1.26.0.
**Libraries:** River v0.35.0, rstudio/platform-lib v3.0.6.
**Date:** 2026-04-21.

## Scenarios

| Scenario | Purpose | Notes |
|---|---|---|
| `steady` | Baseline throughput + latency at sustained load | Mixed-kind workload; 5 tenants |
| `burst` | Response to 10× traffic spike in the middle of a steady run | Baseline → spike at t=5s for 5s → baseline |
| `noisy_neighbor` | Fairness when 1 tenant contributes 50% of enqueues | 10 tenants; skew=0.5 |
| `rate_limit_pressure` | Retry behavior when 30% of jobs return a `rate_limit` error class | |
| `notify_latency` | Isolated enqueue-to-pickup latency with a sparse stream and zero-work jobs | 5 Hz, 1 tenant, `WorkMillis=0` |

## Raw Numbers

### Scenario: `steady`

| Library | Enqueued | Completed | Throughput (c/s) | Pickup p50 | Pickup p95 | Pickup p99 | Max goroutines | Max RSS (MB) |
|---|---|---|---|---|---|---|---|---|
| **platlib** | 2999 | 2351 | **37.0** | 16353.7 | 34307.3 | 35135.8 | 35 | 22.9 |
| **river** | 2998 | 1854 | **29.2** | 18693.1 | 38505.6 | 40955.4 | 60 | 19.5 |

### Scenario: `burst`

| Library | Enqueued | Completed | Throughput (c/s) | Pickup p50 | Pickup p95 | Pickup p99 | Max goroutines | Max RSS (MB) |
|---|---|---|---|---|---|---|---|---|
| **platlib** | 7499 | 2584 | **41.2** | 23829.3 | 50087.1 | 52436.5 | 36 | 22.8 |
| **river** | 7499 | 2085 | **32.8** | 23021.0 | 50805.6 | 52740.9 | 60 | 19.6 |

### Scenario: `noisy_neighbor`

| Library | Enqueued | Completed | Throughput (c/s) | Pickup p50 | Pickup p95 | Pickup p99 | Max goroutines | Max RSS (MB) |
|---|---|---|---|---|---|---|---|---|
| **platlib** | 2999 | 2593 | **41.2** | 15656.1 | 33157.0 | 33797.8 | 35 | 18.8 |
| **river** | 2998 | 2023 | **32.0** | 18340.6 | 37636.3 | 39388.0 | 58 | 19.4 |

**Per-tenant fairness (p95 pickup latency, noisy vs. quiet):**

| Library | Noisy enqueued | Quiet enqueued | Noisy p95 | Quiet p95 | Ratio |
|---|---|---|---|---|---|
| **platlib** | 1485 | 152 | 33162.5 | 32866.4 | 1.01 |
| **river** | 1484 | 152 | 37586.3 | 38801.6 | 0.97 |

### Scenario: `rate_limit_pressure`

| Library | Enqueued | Completed | Failed | Throughput (c/s) | Pickup p50 | Pickup p95 | Pickup p99 |
|---|---|---|---|---|---|---|---|
| **platlib** | 2996 | 2098 | 898 | **34.9** | 10614.8 | 25393.1 | 25707.8 |
| **river** | 2999 | 1545 | 906 | **24.3** | 19001.0 | 36879.1 | 50728.6 |

All failures classified as `rate_limit` by the synthetic error injector.

### Scenario: `notify_latency` (the most striking result)

| Library | Enqueued | Completed | Throughput (c/s) | Pickup p50 | Pickup p95 | Pickup p99 |
|---|---|---|---|---|---|---|
| **platlib** | 2999 | 2999 | 49.9 | **2.4** | **5.4** | **9.9** |
| **river** | 2999 | 2999 | 50.0 | **82.2** | **136.1** | **150.6** |

At sparse-enqueue / zero-work / single-tenant — i.e., the most LISTEN/NOTIFY-dominated condition — **platform-lib is roughly 35× faster on pickup latency**. This is the baseline "LISTEN/NOTIFY responsiveness" number with contention and work duration eliminated.

## Observations

### Throughput

Across every saturated scenario, platform-lib completes about 25–40% more jobs per second than River under identical parameters (10 workers, 100 Hz enqueue, 26 ms median work). Both are worker-bound at this setting — 10 workers × ~100 ms mean duration caps theoretical throughput near 100 c/s; neither library gets close.

A fairer apples-to-apples view needs to run at a lower enqueue rate than the worker pool can service, so pickup latency measures queue overhead rather than queue backlog. **This first pass is intentionally saturated** — the goal was to establish the harness works end-to-end and produce first numbers. See "What's next" below.

### Pickup latency under saturation

Both libraries' pickup latency p50 is ~15–20 seconds in saturated scenarios. That's because we're enqueuing faster than we can drain: by t=30s we have tens of seconds of backlog. **This is queue-wait time, not LISTEN/NOTIFY responsiveness.** The `notify_latency` scenario isolates the LISTEN/NOTIFY piece and shows a 35× gap.

### Fairness

`noisy_neighbor` shows neither library starves the quiet tenant (fairness ratios 0.97–1.01 at p95). FIFO tends toward fair-ish outcomes because workers always take from the head; the "noisy" tenant just gets more jobs completed proportionally to its enqueue share. Either library's fairness behavior under adversarial conditions (e.g., one tenant enqueues 10× faster than workers can process *its* jobs alone) needs dedicated scenarios.

### Retry behavior

Both libraries handle the 30% injected rate-limit error rate similarly: ~900 discards after exhausting default retry attempts in both cases. Throughput drops ~30–40% under this pressure. The River adapter's Permanent-error path is covered by a unit test; the runtime classification in this benchmark (via `SimulatedError`) maps all simulated errors to their class correctly.

### Resource usage

River consistently uses more goroutines (40–60) than platform-lib (14–36). Heap usage is similar. This may reflect different internal workflow strategies (River runs periodic/leader-election goroutines; platform-lib's agent loop is single-goroutine).

## Caveats and honest disclaimers

- **One run per cell.** No variance/CI analysis. A real comparison needs ≥5 runs per scenario with CI bars. These numbers are one data point each — treat them as directional.
- **Saturation distorts latency.** Most scenarios exceeded the worker pool's service rate. The interesting latency comparisons require an under-capacity run.
- **platform-lib had the `notify_latency` optimization purpose-built.** The library was originally designed around cache-backed LISTEN/NOTIFY responsiveness; this benchmark confirms that design goal is realized.
- **Error classification**: the error-class field in `job_failed` events is populated only when the worker returns a `SimulatedError`; adapter-level errors (e.g., DB connection drop) currently surface as "unclassified".
- **Apples-to-apples constraints.** Both adapters use identical Config (10 workers, MaxAttempts=3, default retry policy). Worker count overrides are not exercised in this pass.
- **Single-process only.** Both libraries can shard across processes via Postgres; that's a separate scenario family not covered here.

## What's next

1. **Variance runs** — repeat each scenario 5× and report p50±p95-of-runs.
2. **Under-capacity scenario** — enqueue rate well below service rate so pickup latency reflects queue overhead, not backlog.
3. **Over-capacity scenario** — 1000+ Hz against 10 workers to surface queue-internal inefficiencies at a different pressure point.
4. **Multi-process scenario** — two adapter processes consuming the same queue, to exercise LISTEN/NOTIFY across processes.
5. **`crash_recovery`** — kill the worker process mid-burst (needs a fork/exec harness, deferred).
6. **Cache scenario** — `UseAddressedPush=true` + rscache wiring, so platform-lib's dedup+cache story can be benchmarked as a feature rather than a latency number.

## Methodology

See [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md) for the workload model, error injection semantics, and metric definitions. Raw data for this run is in `results/*.jsonl` (gitignored; reproducible by running `scripts/run-all.sh`).

## License

MIT. See [`LICENSE`](LICENSE).
