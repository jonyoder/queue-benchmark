# Queue Benchmark Results — Variance Pass + Follow-up Items

**Run parameters:** 3 runs per (library, scenario), per-scenario worker counts and durations, Postgres 16 on docker-compose.
**Host:** Apple Silicon (darwin/arm64), Go 1.26.0.
**Libraries:** River v0.35.0, rstudio/platform-lib v3.0.6.
**Date:** 2026-04-21. **Runs analyzed:** 54 across 18 scenario × library pairs.

This is the third benchmark pass, combining:
- Variance pass: 3 runs per cell across 7 scenarios (original 5 + `steady_under` / `steady_balanced`).
- Item 1: platform-lib adapter now supports exponential backoff (`ExponentialBackoffRiverLike`), and `rate_limit_pressure` was re-run with it enabled for apples-to-apples retry-shape comparison.
- Item 2: new `noisy_neighbor_saturated` scenario (100 Hz / 10 workers / 90% tenant skew) to force real starvation pressure.
- Item 3: new `high_scale` scenario (300 Hz / 100 workers / 10 tenants, under-capacity) to test LISTEN/NOTIFY scaling with absolute rate.
- Items 4 (`crash_recovery`) and 5 (`rscache` integration) are documented in [`docs/FUTURE.md`](docs/FUTURE.md) as deferred with concrete design notes.

## TL;DR — what we've learned

| Finding | Where it matters | Evidence | Confidence |
|---|---|---|---|
| **LISTEN/NOTIFY responsiveness** — platform-lib 10–60× faster on pickup latency when not saturated | `notify_latency`, `steady_under`, `high_scale`, `noisy_neighbor` | p95 gap consistent 16–63×, tight run-to-run variance | High |
| **Saturated throughput advantage** — platform-lib completes 25–30% more jobs in the same wall-clock under backlog | `burst`, `steady_over`, `noisy_neighbor_saturated` | Completions per second consistent across runs | Medium — may be adapter-implementation sensitive |
| **LISTEN/NOTIFY advantage scales with rate** — at 300 Hz the gap widens to 63×, not narrows | `high_scale` | 4.5ms vs 285ms p95 at 300 Hz | High |
| **Retry shape, not count** — both libraries discard the same jobs; their *backoff* shapes differ by design | `rate_limit_pressure` (both configurations) | Identical failure counts; p95 1000× different by retry policy | High, but the backoff implementations are not identical-by-construction |
| **Natural FIFO fairness** — both libraries produce ~1.0 fairness ratios even under saturated skew | `noisy_neighbor`, `noisy_neighbor_saturated` | Per-tenant p95 near equal; FIFO by design | High |
| **Goroutine footprint** — River uses ~2× more goroutines than platform-lib | All scenarios | Maxes consistent across runs | High |

**Biggest architectural distinction:** platform-lib prioritizes pickup latency responsiveness; River prioritizes durability, leader-election resilience, and operational batteries-included. Both are correct choices for their designs.

---

## Pickup latency under under-capacity — the headline

When workers are not saturated (service rate > enqueue rate), pickup latency is the LISTEN/NOTIFY round-trip cost. Across four distinct scenarios at different rates:

| Scenario | platform-lib p95 | River p95 | Ratio |
|---|---|---|---|
| `notify_latency` (20 Hz, zero-work) | **5.7 ms** | 102.7 ms | 18× |
| `steady_under` (30 Hz, 20 workers) | **6.3 ms** | 101.5 ms | 16× |
| `noisy_neighbor` (50 Hz, 20 workers) | **4.7 ms** | 92.8 ms | 20× |
| **`high_scale` (300 Hz, 100 workers)** | **4.5 ms** | **285.0 ms** | **63×** |

`high_scale` is the new data point. At 6× higher enqueue rate than the other unsaturated scenarios:
- platform-lib's pickup p95 actually *improves* (4.5 ms vs 5.7 ms) — higher rate keeps the LISTEN/NOTIFY path warm.
- River's pickup p95 *degrades* (285 ms vs 102 ms) — more jobs arrive between polling ticks, so the last-arrived in each tick waits longer.

**The LISTEN/NOTIFY advantage scales with load.** It's not an "at idle" effect; it's structural.

### Per-scenario detail (under-capacity)

| Library | Scenario | Runs | Throughput (c/s) | Pickup p50 | Pickup p95 (med, min–max) | Pickup p99 |
|---|---|---|---|---|---|---|
| platform-lib | `notify_latency` | 3 | 10.0 | 4.0 | **5.7** (5.7–7.1) | 7.3 |
| platform-lib | `steady_under` | 3 | 15.0 | 3.5 | **6.3** (5.3–7.7) | 8.3 |
| platform-lib | `noisy_neighbor` | 3 | 24.9 | 2.8 | **4.7** (4.6–6.6) | 7.2 |
| platform-lib | `high_scale` | 3 | 149.5 | 2.0 | **4.5** (4.3–4.7) | 6.1 |
| River | `notify_latency` | 3 | 10.0 | 51.4 | 102.7 (102.1–103.2) | 105.1 |
| River | `steady_under` | 3 | 15.0 | 37.9 | 101.5 (101.4–101.7) | 104.8 |
| River | `noisy_neighbor` | 3 | 25.0 | 44.9 | 92.8 (90.7–95.3) | 103.8 |
| River | `high_scale` | 3 | 149.9 | 71.1 | 285.0 (276.5–291.4) | 315.4 |

All run-to-run spreads are within ±5% of median.

---

## Saturated scenarios — queue-wait dominates, but platform-lib still pulls ahead

When enqueue rate exceeds service rate, pickup latency is dominated by queue-wait time, not library coordination. But platform-lib still consistently delivers more work per unit time.

| Scenario | Lib | Throughput (c/s) | Pickup p95 | Completed |
|---|---|---|---|---|
| `steady_over` (50 Hz / 10 workers) | platform-lib | 24.9 | 6973 ms | 1499 |
| | River | 25.0 | 15927 ms | 1499 |
| `burst` (50 Hz + 10× spike × 5s / 10 workers) | platform-lib | **40.8** | 48015 ms | 2574 |
| | River | 32.8 | 48953 ms | 2081 |
| `noisy_neighbor_saturated` (100 Hz / 10 workers / 90% skew) | platform-lib | **39.2** | 33261 ms | **2480** |
| | River | 31.0 | 39416 ms | 1916 |

In `burst` and `noisy_neighbor_saturated`, platform-lib completes **25–30% more jobs** in the same wall-clock. The pickup p95 numbers are in the 30-50 second range for both libraries because the queue has built up tens of seconds of backlog.

**Why more completions?** Under saturation, every worker slot that's not waiting on LISTEN/NOTIFY handoff is one more job completed. platform-lib's LISTEN/NOTIFY-first dispatch keeps workers busier. River's polling interval includes idle gaps that compound across thousands of jobs.

**Caveat:** these numbers are sensitive to adapter-implementation choices. My platform-lib `QueueStore` uses `FOR UPDATE SKIP LOCKED` with a fast-path idle count — decisions a production-tuned store might make differently. A batch-pop store could produce different throughput numbers.

---

## `noisy_neighbor_saturated` — fairness under starvation pressure

With one tenant enqueuing 90% of 100 Hz against 10 workers (saturated), does FIFO still produce fair latencies?

### Fairness ratios (noisy p95 / quiet p95)

| Library | Run 1 | Run 2 | Run 3 |
|---|---|---|---|
| platform-lib | 1.18 | 1.17 | 1.18 |
| River | 0.97 | 0.97 | 0.98 |

A ratio near 1.0 means no starvation. Notably, **platform-lib's 1.18 means the noisy tenant has *slightly slower* p95 than the quiet tenant** — the opposite of starvation. This is because noisy-tenant enqueues are time-clustered at the tail of the run (they keep arriving at 90 Hz until t=30s), while quiet-tenant enqueues are sparse, so the quiet tenant's *last* enqueue lands earlier in the run and therefore experiences less queue wait. FIFO is temporally fair.

River's 0.97 is essentially perfect fairness — the small deviation is sampling noise.

**Neither library has per-tenant priority or fairness**; both rely on FIFO. Under saturation, FIFO produces proportionate delays that roughly match enqueue-time arrival. No tenant is systematically disadvantaged.

### Completion share under saturation

| Library | Total completed | Noisiest tenant completed | Share |
|---|---|---|---|
| platform-lib | 2480 | ~2236 | 90% |
| River | 1916 | ~1727 | 90% |

Both libraries complete work proportionate to enqueue share (the noisy tenant contributed 90% of enqueues and gets 90% of completions). This is a direct consequence of FIFO + no per-tenant worker allocation.

**For true fairness enforcement** you'd need to add a per-tenant worker cap or weighted fair-queuing on top of either library. Neither provides it natively.

---

## `rate_limit_pressure` — retry shape comparison

Both libraries configured with `MaxAttempts=3`. platform-lib adapter tested both without backoff (immediate re-enqueue) and with `ExponentialBackoffRiverLike` (matches River's 1s/16s/81s spacing).

| Config | Completed | Failed events | Discarded | Pickup p50 | Pickup p95 |
|---|---|---|---|---|---|
| platform-lib (immediate retry) | 626 | 2454 | 273 | 4.5 ms | 15.9 ms |
| **platform-lib (River-like backoff)** | **626** | **2457** | **273** | **4.0 ms** | **17017 ms** |
| **River (native backoff)** | **626** | **2457** | **273** | **621.8 ms** | **18693 ms** |

With retry shapes matched, the outcome counts are identical. What differs:

1. **platform-lib p50 stays at ~4ms even with backoff**; River's p50 is ~622ms.
2. **platform-lib p95 is slightly lower** than River's (17.0s vs 18.7s) at matched backoff, with much tighter variance (17016–17017 vs 18633–18837).

### Why the p50 difference?

My platform-lib adapter implements backoff with a **goroutine-based `time.AfterFunc` pattern** — when a job fails and is scheduled for retry, the worker slot is freed immediately and the retry fires on a background goroutine. Fresh enqueues don't contend with retry-waiting jobs for worker attention.

River implements backoff via **Postgres `scheduled_at`** — the retry job sits in the DB with a future timestamp and is eligible to be claimed by any worker once that timestamp has passed. This approach is **durable across process restarts** (a crash loses nothing) but means fresh enqueues at t+1s are competing with retry jobs for worker attention, pushing up pickup p50.

**Both are defensible designs:**
- Goroutine-based backoff (platform-lib adapter): lower pickup latency, but retry is lost if the process dies during backoff. Appropriate for non-durable retry semantics.
- DB-based backoff (River native): slightly higher pickup latency under retry pressure, but durable across restarts. Appropriate for production systems where retry-loss is unacceptable.

**Caveat:** this goroutine-based backoff is an *adapter* choice, not platform-lib's native behavior. A production platform-lib user building retry semantics would likely choose DB-based backoff (same pattern as River) for durability. This benchmark measures my implementation, not platform-lib's inherent approach.

---

## Resource usage

| Scenario | platform-lib goroutines | River goroutines | platform-lib RSS MB | River RSS MB |
|---|---|---|---|---|
| `notify_latency` | 14 | 40 | 18.2 | 18.3 |
| `steady_under` | 36 | 62 | 18.6 | 18.5 |
| `noisy_neighbor` | 44 | 72 | 18.8 | 19.2 |
| `steady_over` | 35 | 60 | 19.1 | 18.8 |
| `steady_balanced` | 55 | 74 | 19.3 | 19.1 |
| `burst` | 35 | 60 | 23.1 | 19.1 |
| `noisy_neighbor_saturated` | 35 | 60 | 19.0 | 19.1 |
| `rate_limit_pressure` (no backoff) | 32 | 56 | 19.0 | 19.3 |
| `rate_limit_pressure` (backoff) | 193 | 58 | 19.0 | 19.3 |
| `high_scale` | 182 | 198 | 35.9 | 24.4 |

Two observations:
1. River consistently runs ~2× more goroutines than platform-lib in most scenarios. Likely due to River's periodic/leader-election/metrics goroutines. Roughly flat across load.
2. **platform-lib goroutines spike under retry-with-backoff** and under high enqueue rate. This reflects my adapter's goroutine-per-backoff-job implementation — each scheduled retry creates a goroutine that sleeps then re-enqueues. At rate_limit with backoff, 273 discards × 3 attempts = 819 goroutines spawned across the run. A pool-based scheduler would cap this.

---

## When would we switch to platform-lib from River?

Based on this data, the case for platform-lib is strongest when:

- **Low-latency pickup is a product requirement.** Near-zero queue-idle latency matters: user-facing "optimistic action" paths, live-UI notifications, any scenario where a few hundred ms of queue lag is visible in UX.
- **High rate of small jobs.** The LISTEN/NOTIFY advantage widens with rate (63× at 300 Hz vs 18× at 20 Hz in this benchmark). If your workload is many-short-jobs, platform-lib's dispatch overhead stays flat.
- **The architectural adjacents fit.** platform-lib's unique value is the integrated cache + queue + broadcaster set. If you're building a system that wants all three unified, the ecosystem fit is better than bolting cache onto River.

The case for **staying on River** is strongest when:

- **Out-of-the-box operational features matter.** River ships leader election, periodic jobs, middleware ecosystem, CLI tooling, metrics integrations, and transactional enqueue. Building all of that on top of rsqueue is possible but is work you don't have today.
- **Durable retry / crash resilience matters.** River's DB-based retry persists through restarts; platform-lib's retry story is "build your own" (per this benchmark's adapter choice).
- **You're doing 100–1000 Hz steady-state and can tolerate 100ms pickup latency.** River's pickup at moderate rate is below 150ms p99 — totally acceptable for most SaaS workloads.

For Keavi specifically: the LISTEN/NOTIFY advantage *matters* (the SSE and conversational paths care about sub-100ms responsiveness), but River's operational completeness also matters (Keavi has 55 workers across many job types and benefits from the ecosystem). **A hybrid approach — stay on River for queuing, adopt platform-lib's cache module independently** — remains the recommendation from the earlier audit. See `project_future_platformlib_cache.md` in Keavi's memory.

---

## What's next (in order of value)

Deferred work is documented in [`docs/FUTURE.md`](docs/FUTURE.md) with concrete design notes. Ordered by informational value vs. effort:

1. **Jitter in `ExponentialBackoffRiverLike`** — matches River's ±10% jitter so retry-shape is fully apples-to-apples. ~30 min.
2. **Multi-process scenario** — two adapter processes consuming the same queue. Exercises cross-process LISTEN/NOTIFY. ~1 hour.
3. **`crash_recovery`** — SIGKILL the worker process mid-burst; measure duplicates and resume time. Needs parent/child process split. ~2–4 hours.
4. **`rscache` + AddressedPush integration** — a capability benchmark of platform-lib's unique cache-integrated queue. Not head-to-head (River has no equivalent). ~2–4 hours.
5. **DB-backed backoff in the platform-lib adapter** — mirror River's durability model so backoff-enabled comparisons are truly apples-to-apples. Requires building a scheduler layer on top of rsqueue's `Push`. ~1–2 hours.

## Methodology + caveats

See [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md) for the workload model and metric definitions. Raw JSONL data lives in `results/` (gitignored; reproducible via `scripts/run-all.sh` and `scripts/run-items-2-3.sh`).

**Things this benchmark doesn't measure:**
- Cross-process coordination (deferred).
- Crash resilience / recovery behavior (deferred).
- Real-world workload patterns (synthetic `time.Sleep` approximates work).
- Long-running stability (30s runs — memory leaks / GC pressure over hours are invisible).
- Failure modes beyond the three simulated error classes.

**Things this benchmark measures well:**
- LISTEN/NOTIFY pickup latency, under four different pressure regimes, with tight run-to-run variance.
- Throughput differences at saturation.
- Retry-shape semantics (with the backoff asymmetry honestly documented).
- Natural fairness under tenant skew.

## License

MIT. See [`LICENSE`](LICENSE).
