# Future Work

Deliberately-deferred items from the autonomous sprint on 2026-04-21. Each is feasible and well-scoped but exceeds the bounded session budget. The design notes here are intended to let a future session pick up each item without re-discovering the shape of the work.

## `crash_recovery` scenario

**Goal:** measure how each library behaves when its worker process is killed mid-burst. Metrics:

- Duplicate execution count (jobs that ran to completion twice because the first attempt died after the worker started but before the queue cleaned up).
- Time-to-resume (seconds from process kill to the first worker pickup of a new or in-flight job after a fresh process starts).
- Data-integrity surface (jobs whose state cannot be disambiguated: "did this run or not?").

**Why it's harder than the other scenarios:** the benchmark harness today is a single process. Crash recovery requires a parent/child split — the parent runs the JSONL recorder and workload generator; the child runs the workers; the parent SIGKILLs the child mid-run and relaunches.

**Suggested design:**

- Add `--mode=parent|child` to `cmd/bench`.
- Parent: records run_start, spawns child as `bench run --mode=child ...`, generates workload, kills child at T+N seconds via `cmd.Process.Kill()`, respawns child, waits for child to drain, records run_end.
- Child: reads workload from an IPC channel (pipe or shared Postgres table), processes via the Harness, writes per-job events to a shared file or pipes them back to the parent.
- JSONL continuity: the parent appends child events to the run's single JSONL. The recorder accepts external events as well as the parent's own.
- Duplicate detection: jobs with the same `job_id` that fire `job_completed` more than once.
- Simplification: instead of full IPC, use a shared Postgres table as the "inbox" (parent enqueues work), same as rsqueue/River do natively; the benchmark becomes "how fast does a new worker pool resume pulling from the shared queue after the old one dies?"

**Budget estimate:** 2–4 hours.

## rscache + AddressedPush integration

**Goal:** benchmark platform-lib's **unique architectural feature** — queue-integrated caching via `AddressedPush` + rscache + rsstorage. Scenarios:

- `cache_dedup`: many enqueues target a small set of addresses. First enqueue triggers work; subsequent enqueues return the cached result without re-running. Measures cache hit rate and the effective cost reduction.
- `cache_concurrent_miss`: many simultaneous enqueues for the same missing address. Measures how the cache's dedup path prevents duplicate work.

**Note:** this is NOT a head-to-head library comparison because River does not have an equivalent queue-integrated cache. The benchmark here measures what platform-lib can do that's architecturally distinct.

**Suggested design:**

- `platlib.Options.WithCache(cache rscache.FileCache)` — optional cache wiring.
- Construct `cacheQueueWrapper` around the underlying `queue.Queue`.
- Construct `rsstorage.NewFileStorageServer` pointing at a temp dir.
- Wire `rscache.NewFileCache` with the wrapper + a `DuplicateMatcher` that recognizes `queue.ErrDuplicateAddressedPush`.
- New `scenario=cache_dedup` where `ConcurrentHits` jobs target `N` unique addresses; measure unique executions vs. total enqueue count.

**Budget estimate:** 2–4 hours, dominated by understanding rscache's contract around cached-result resolution.

## Other

### `burst` scenario re-tuning

Current `burst` multiplier is 10× for 5s in a 30s run, producing 3750 enqueues against 10 workers — which is total saturation. A lower multiplier (e.g., 3× for 10s) against 20 workers would be a more realistic "spike within capacity" test showing transient queue behavior without permanent backlog.

### Post-enqueue idempotency test

Can we de-duplicate jobs within a single benchmark run at the application layer (e.g., per-address dedup) to verify both libraries' semantics for duplicate enqueue? River has `UniqueOpts`; platform-lib has `AddressedPush`. A symmetric test would compare their dedup guarantees under concurrent enqueue.

### Multi-process throughput

Run two adapter processes consuming the same Postgres queue. Measures whether LISTEN/NOTIFY + `FOR UPDATE SKIP LOCKED` coordination scales across process boundaries. Simpler than `crash_recovery` because processes are cooperative, not fault-injecting.

### Jitter in retry backoff

`ExponentialBackoffRiverLike` matches River's base policy but without jitter. River adds ±10% jitter to avoid synchronized retries. Adding jitter to the platlib adapter would make retry-shape comparisons fully apples-to-apples.
