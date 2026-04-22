package workload

import (
	"fmt"
	"time"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
)

// Scenario enumerates the named scenarios that ship with the benchmark.
type Scenario string

const (
	ScenarioSteady            Scenario = "steady"
	ScenarioBurst             Scenario = "burst"
	ScenarioNoisyNeighbor     Scenario = "noisy_neighbor"
	ScenarioRateLimitPressure Scenario = "rate_limit_pressure"
	ScenarioNotifyLatency     Scenario = "notify_latency"
)

// SpecFor returns a Spec for the named scenario, with baseline parameters
// chosen to exercise each scenario's distinguishing feature at a modest
// scale suitable for integration smoke tests. Scenarios that benchmark
// real scaling apply env-var overrides on top of these defaults in the CLI.
func SpecFor(name Scenario) (Spec, error) {
	switch name {
	case ScenarioSteady:
		return Spec{
			Name:         string(name),
			Duration:     10 * time.Second,
			Tenants:      5,
			TenantSkew:   0.0,
			EnqueueRate:  50,
			JobMix:       defaultMix(),
			Seed:         1,
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

	case ScenarioNotifyLatency:
		return Spec{
			Name:        string(name),
			Duration:    10 * time.Second,
			Tenants:     1,
			EnqueueRate: 5, // sparse — one job at a time
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
		ScenarioSteady,
		ScenarioBurst,
		ScenarioNoisyNeighbor,
		ScenarioRateLimitPressure,
		ScenarioNotifyLatency,
	}
}
