package workload

import (
	"fmt"
	"time"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
)

// Scenario enumerates the named scenarios that ship with the benchmark.
type Scenario string

const (
	ScenarioSteadyUnder           Scenario = "steady_under"             // low enqueue rate, excess workers
	ScenarioSteadyBalanced        Scenario = "steady_balanced"          // moderate rate, matched capacity
	ScenarioSteadyOver            Scenario = "steady_over"              // saturated, workers bottleneck
	ScenarioBurst                 Scenario = "burst"
	ScenarioNoisyNeighbor         Scenario = "noisy_neighbor"
	ScenarioNoisyNeighborSat      Scenario = "noisy_neighbor_saturated" // 90% skew + saturation
	ScenarioRateLimitPressure     Scenario = "rate_limit_pressure"
	ScenarioNotifyLatency         Scenario = "notify_latency"
	ScenarioHighScale             Scenario = "high_scale" // high absolute rate + high workers, under-capacity

	// ScenarioSteady is kept as an alias for steady_over to preserve
	// backward compatibility with existing results files.
	ScenarioSteady Scenario = "steady"
)

// SpecFor returns a Spec for the named scenario, with baseline parameters
// chosen to exercise each scenario's distinguishing feature at a modest
// scale suitable for integration smoke tests. Scenarios that benchmark
// real scaling apply env-var overrides on top of these defaults in the CLI.
func SpecFor(name Scenario) (Spec, error) {
	switch name {
	case ScenarioSteady, ScenarioSteadyOver:
		return Spec{
			Name:         string(name),
			Duration:     10 * time.Second,
			Tenants:      5,
			TenantSkew:   0.0,
			EnqueueRate:  50,
			JobMix:       defaultMix(),
			Seed:         1,
		}, nil

	case ScenarioSteadyUnder:
		// Enqueue rate is well below theoretical worker capacity so pickup
		// latency reflects LISTEN/NOTIFY overhead + job-dispatch path,
		// not queue backlog. Use with --workers >= 20 to actually be
		// under-capacity for the mixed-kind workload's mean duration.
		return Spec{
			Name:        string(name),
			Duration:    10 * time.Second,
			Tenants:     5,
			EnqueueRate: 30,
			JobMix:      defaultMix(),
			Seed:        10,
		}, nil

	case ScenarioSteadyBalanced:
		// Moderate pressure: workers are typically ~50% utilized. Neither
		// in idle LISTEN/NOTIFY territory nor saturated backlog.
		return Spec{
			Name:        string(name),
			Duration:    10 * time.Second,
			Tenants:     5,
			EnqueueRate: 80,
			JobMix:      defaultMix(),
			Seed:        11,
		}, nil

	case ScenarioBurst:
		return Spec{
			Name:        string(name),
			Duration:    15 * time.Second,
			Tenants:     5,
			EnqueueRate: 50,
			RateCurve:   Burst(5*time.Second, 5*time.Second, 10.0),
			JobMix:      defaultMix(),
			Seed:        2,
		}, nil

	case ScenarioNoisyNeighbor:
		return Spec{
			Name:        string(name),
			Duration:    10 * time.Second,
			Tenants:     10,
			TenantSkew:  0.5,
			EnqueueRate: 50,
			JobMix:      defaultMix(),
			Seed:        3,
		}, nil

	case ScenarioRateLimitPressure:
		return Spec{
			Name:        string(name),
			Duration:    10 * time.Second,
			Tenants:     5,
			EnqueueRate: 30,
			JobMix:      defaultMix(),
			ErrorInjection: map[qpkg.ErrorClass]float64{
				qpkg.ErrorRateLimit: 0.3,
			},
			Seed: 4,
		}, nil

	case ScenarioNoisyNeighborSat:
		// One tenant dominates 90% of enqueues against a queue that's
		// saturated (100 Hz against 10 workers @ ~100ms mean duration =
		// above capacity). This is the real fairness test: with limited
		// worker slots, does one tenant's burst crowd out the others?
		return Spec{
			Name:        string(name),
			Duration:    10 * time.Second,
			Tenants:     10,
			TenantSkew:  0.9,
			EnqueueRate: 100,
			JobMix:      defaultMix(),
			Seed:        12,
		}, nil

	case ScenarioHighScale:
		// High absolute enqueue rate at modestly-under-capacity so
		// LISTEN/NOTIFY can still matter but Postgres coordination
		// overhead also comes into play. 300 Hz against 100 workers
		// @ ~100ms mean = ~30% utilization.
		return Spec{
			Name:        string(name),
			Duration:    15 * time.Second,
			Tenants:     10,
			EnqueueRate: 300,
			JobMix:      defaultMix(),
			Seed:        13,
		}, nil

	case ScenarioNotifyLatency:
		// Sparse zero-work enqueues exercise the LISTEN/NOTIFY path in
		// isolation — no worker contention, no work to time. 20 Hz gives
		// ~600 samples in 30s for stable p99.
		return Spec{
			Name:        string(name),
			Duration:    10 * time.Second,
			Tenants:     1,
			EnqueueRate: 20,
			JobMix: []JobClass{
				{Kind: "entity_update", Weight: 1, WorkMillisMin: 0, WorkMillisMax: 0},
			},
			Seed: 5,
		}, nil
	}

	return Spec{}, fmt.Errorf("unknown scenario %q", name)
}

// defaultMix is a reasonable mixed-kind workload used by most scenarios.
// Percentages roughly match patterns common in multi-tenant SaaS systems:
// majority short DB-ish jobs, some medium, a tail of long external-API jobs.
func defaultMix() []JobClass {
	return []JobClass{
		{
			Kind: "entity_update", Weight: 60,
			WorkMillisMin: 5, WorkMillisMax: 30,
			PayloadBytesMin: 128, PayloadBytesMax: 512,
		},
		{
			Kind: "tenant_snapshot", Weight: 20,
			WorkMillisMin: 20, WorkMillisMax: 150,
			PayloadBytesMin: 256, PayloadBytesMax: 1024,
		},
		{
			Kind: "entity_enrich", Weight: 15,
			WorkMillisMin: 100, WorkMillisMax: 800,
			PayloadBytesMin: 256, PayloadBytesMax: 1024,
		},
		{
			Kind: "document_process", Weight: 5,
			WorkMillisMin: 1000, WorkMillisMax: 5000,
			PayloadBytesMin: 1024, PayloadBytesMax: 4096,
		},
	}
}

// AllScenarios returns every built-in scenario name.
func AllScenarios() []Scenario {
	return []Scenario{
		ScenarioSteadyUnder,
		ScenarioSteadyBalanced,
		ScenarioSteadyOver,
		ScenarioBurst,
		ScenarioNoisyNeighbor,
		ScenarioNoisyNeighborSat,
		ScenarioRateLimitPressure,
		ScenarioNotifyLatency,
		ScenarioHighScale,
	}
}
