# queue-benchmark

A harness for benchmarking Postgres-backed Go job queue libraries under workloads representative of multi-tenant SaaS background processing.

## Motivation

Job queues are the backbone of most production SaaS systems. Their performance under realistic workloads — not synthetic stress tests — determines how well a service handles bursts, how fairly tenants share resources, and how much operational pain the team lives with. Vendor benchmarks tend to measure throughput under idealized conditions; this project measures behavior under conditions that actually appear in production.

This benchmark is vendor-agnostic and intended to produce reproducible, peer-reviewable data. Current libraries under evaluation:

- [River](https://github.com/riverqueue/river) — a production-oriented Postgres-backed Go queue with LISTEN/NOTIFY, transactional enqueue, and native worker pools.
- [rstudio/platform-lib](https://github.com/rstudio/platform-lib) — a set of platform-grade libraries including a Postgres-backed queue with tiered caching integration.

Additional libraries may be added in the future.

## What this benchmark measures

- **Throughput ceilings** — sustained jobs-per-second before the DB becomes the bottleneck.
- **Latency distribution** — enqueue-to-pickup and enqueue-to-completion p50/p95/p99, under both steady state and burst conditions.
- **Fairness under noisy neighbors** — how one tenant's job spike affects other tenants' latency.
- **Recovery characteristics** — behavior when worker processes crash mid-job: duplicate-run count, time to resume, data-integrity risk surface.
- **Notification responsiveness** — LISTEN/NOTIFY enqueue-to-worker-pickup minimum latency.
- **Database load** — queries per second against the queue tables under each scenario.
- **Memory footprint** — resident memory per 1k concurrent in-flight jobs.

## Workload model

The benchmark simulates a generic multi-tenant SaaS. Workers are grouped by duration profile (short / medium / long), external-dependency profile (CPU-bound / DB-bound / external-API-bound), and tenant scope (per-tenant / system-wide).

See [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md) for the full workload taxonomy and measurement methodology.

Scenarios include:

- **Steady state** at various throughput levels.
- **Burst** — 10× normal traffic for 60 seconds.
- **Noisy neighbor** — one tenant contributes 50% of total enqueues.
- **Crash recovery** — SIGKILL the worker process mid-burst; measure duplicate-execution and time-to-resume.
- **External provider rate-limit** — workers return 429-class errors; measure retry behavior and effective throughput.
- **LISTEN/NOTIFY responsiveness** — isolated measurement of the enqueue → worker-pickup minimum latency.

## Usage

### Prerequisites

- Go 1.25+
- Docker + docker-compose

### Running a scenario

```bash
make up                              # start ephemeral Postgres
make bench LIB=river SCENARIO=steady # run a scenario
make report                          # analyze collected data
make down                            # tear down Postgres
```

## Design principles

- **Apples-to-apples**: adapters expose the same workload-runner interface to each library. Configurations are matched as closely as the libraries allow, and asymmetries are documented.
- **Real Postgres**: benchmarks run against an actual Postgres instance in docker-compose, not an in-memory mock. LISTEN/NOTIFY behavior depends on the real broker.
- **Reproducible**: scenario definitions are declarative config files; raw data is written as newline-delimited JSON; analysis is deterministic from raw data.
- **Peer-reviewable**: results include raw data alongside aggregated summaries so anyone can re-analyze or question the methodology.

## Status

Under active development. This README will be updated with a full results report when the first comparison pass completes.

## License

MIT. See [`LICENSE`](LICENSE).

## Contributing

Issues and PRs welcome. Particular interest in:

- Additional library adapters.
- Additional scenarios modeled on real production workloads.
- Corrections to methodology or measurement.
