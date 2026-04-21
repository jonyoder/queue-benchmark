# Methodology

## Workload model

The benchmark simulates a generic multi-tenant SaaS background-job workload. Jobs are organized along three independent dimensions, each calibrated to patterns commonly observed in production systems.

### Duration profile

| Class | Target wall-clock | Realistic workload analog |
|---|---|---|
| Short | <1s | CRUD-style business-logic updates, notifications |
| Medium | 1–30s | Per-tenant aggregations, small ML inference, CSV-row processing |
| Long | 30–300s | External-API calls with variable latency, vision/LLM inference, PDF parsing |

### External-dependency profile

- **CPU-bound** — work happens in-process; bottleneck is goroutine scheduling + Postgres interaction.
- **DB-bound** — significant time in Postgres queries during the job; bottleneck is the DB.
- **External-API-bound** — work is dominated by a blocking network call to a third-party service; errors may include 429 rate-limit responses.

### Tenant scope

- **Per-tenant** — the job belongs to one tenant, and fairness measurements treat it as such.
- **System-wide** — coordinator/cron jobs that either iterate tenants themselves or are unscoped; excluded from per-tenant fairness metrics.

## Job taxonomy

Benchmarks generate jobs of the following generic kinds:

| Kind | Duration | Profile | Tenant-scoped |
|---|---|---|---|
| `document_process` | Long | External-API | Yes |
| `document_batch_analyze` | Long | External-API | Yes |
| `entity_update` | Short | DB | Yes |
| `entity_enrich` | Medium | External-API | Yes |
| `tenant_rollup` | Medium | DB | Yes |
| `tenant_snapshot` | Short | DB | Yes |
| `daily_coordinator` | Short | DB | No (fans out to per-tenant jobs) |
| `monthly_coordinator` | Short | DB | No (fans out) |
| `notification_deliver` | Short | External-API | Yes |

These names are intentionally generic. They describe the job's runtime shape, not any specific application's semantics.

## Scenarios

Each scenario is a declarative configuration specifying:

- Job-kind mix (percentage of each kind in the workload)
- Throughput curve (steady, ramp, burst, etc.)
- Tenant distribution (uniform, skewed, power-law)
- Error injection (what fraction of jobs return which error class)
- Duration of the run

### Built-in scenarios

- **`steady`** — Mixed-kind workload at a configurable throughput level; run for a fixed duration; used to establish baseline numbers.
- **`burst`** — Steady baseline for N seconds, then 10× burst for 60 seconds, then steady again. Measures the queue's response to sudden load.
- **`noisy_neighbor`** — 1 tenant accounts for 50% of enqueues; 99 other tenants share the remaining 50%. Measures tenant-fairness behavior (does the noisy tenant starve others?).
- **`crash_recovery`** — Steady workload; SIGKILL the worker process at T+60s; restart at T+75s. Measures in-flight job handling (duplicate executions, time-to-resume, data integrity surface).
- **`rate_limit_pressure`** — 30% of jobs return `rate_limit` errors for 60 seconds, simulating a provider rate-limit window. Measures how the library's retry policy interacts with the error class.
- **`notify_latency`** — Isolated measurement of enqueue-to-pickup latency: enqueue one job at a time, measure the delay before a worker begins it. Cross-checks Postgres LISTEN/NOTIFY responsiveness.

## Metrics

### Per-job metrics
- Enqueue time
- First-pickup time
- Completion time (per attempt)
- Final status (completed / failed / discarded)
- Attempt count

Derived: latency = pickup - enqueue; total duration = completion - enqueue.

### Per-run aggregates
- Throughput (jobs/sec completed)
- Latency percentiles: p50, p90, p95, p99, p99.9
- Per-tenant latency distribution (for noisy-neighbor analysis)
- Error rate by class
- Duplicate-execution count (for crash-recovery scenarios)

### Resource metrics
- Process RSS sampled at 1 Hz
- DB query count / sec against queue tables
- Worker pool utilization (busy workers / total workers) sampled at 1 Hz

### Fairness metrics
- **Tenant latency ratio**: noisy-tenant p95 / quiet-tenant p95. Higher = less fair.
- **Tenant throughput ratio**: noisy-tenant jobs/sec / quiet-tenant jobs/sec (both normalized by enqueue rate). Expected ~1.0 for perfect fairness; ratio >1 means noisy tenant gets disproportionate throughput.

## Output format

Raw results are newline-delimited JSON, one file per run, named `{run_id}.jsonl`. Each line is an `Event` with shape:

```json
{"run_id": "...", "timestamp": "...", "kind": "job_completed", "fields": {...}}
```

Event kinds include: `run_start`, `run_end`, `job_enqueued`, `job_started`, `job_completed`, `job_failed`, `job_retried`, `job_discarded`, `queue_snapshot`, `resource_sample`. See `internal/metrics` for the schema.

Analysis is deterministic from the raw JSONL — anyone can re-run the analyzer or implement their own against the same data.

## Determinism and reproducibility

- Each run has a deterministic `run_id` based on scenario + library + run ordinal, so results are overwrite-safe across runs.
- Workload-generator randomness is seeded from the scenario config for reproducibility.
- Postgres is an ephemeral container with a clean database at the start of each scenario (no cross-scenario state leakage).
- Hardware is documented in `RESULTS.md` for every published comparison.

## What this benchmark does NOT measure

- Throughput against idealized workloads (e.g., minimum-work jobs enqueued and completed immediately).
- Behavior against non-Postgres backends (file-based, Redis-only, etc.).
- Developer ergonomics (API quality, documentation, debugging experience) — qualitative, not benchmarked here.
- Integration cost with specific frameworks (Gin, Echo, etc.).
- Library-specific features that can't be exercised via the common `Harness` interface.

## Apples-to-apples constraints

Both libraries are configured with matching parameters where possible:
- Same Postgres instance, same connection pool size.
- Same number of workers per queue.
- Same max attempts.
- Same job-payload size distributions.

Asymmetries are documented in `RESULTS.md` under "Configuration differences" for each scenario.
