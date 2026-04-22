// Package workload models the synthetic job stream driving benchmark
// scenarios. Scenarios are declarative Specs; the Generator turns a Spec
// into a stream of EnqueueRequests fed to a Harness.
//
// Design
//
// The generator is deterministic for a given (Spec, Seed) pair so
// results are reproducible across runs. Randomness is seeded per-scenario
// in the Spec so test retries replay identically.
package workload

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
)

// Spec is the declarative description of a scenario's workload.
type Spec struct {
	// Name is the scenario identifier (e.g., "steady", "burst").
	Name string

	// Duration is how long the generator will enqueue jobs.
	Duration time.Duration

	// Tenants is the number of distinct tenant UUIDs to use. Tenant
	// selection is governed by TenantSkew.
	Tenants int

	// TenantSkew is the fraction of all jobs assigned to the noisiest
	// tenant. 0.0 = uniform across all tenants; 0.5 means one tenant
	// accounts for half the jobs. The remaining fraction is uniform
	// across the other tenants.
	TenantSkew float64

	// EnqueueRate is the steady-state arrival rate in jobs per second.
	// Rate curves (burst, ramp) are applied on top of this baseline.
	EnqueueRate int

	// RateCurve optionally modulates EnqueueRate over time. If nil the
	// rate is constant.
	RateCurve RateCurve

	// JobMix lists the job kinds produced. Weights are normalized.
	JobMix []JobClass

	// ErrorInjection maps error class to probability. For each
	// generated job, the generator samples these probabilities (in
	// order: rate_limit, transient, permanent); the first one that
	// hits sets the payload's FailWithError.
	ErrorInjection map[qpkg.ErrorClass]float64

	// Seed controls randomness for reproducibility.
	Seed uint64
}

// JobClass describes one kind of job in a workload mix.
type JobClass struct {
	Kind qpkg.Kind

	// Weight is the relative frequency of this class within JobMix.
	Weight float64

	// WorkMillisMin / Max define the uniform range of synthetic work
	// duration for jobs of this class.
	WorkMillisMin int64
	WorkMillisMax int64

	// PayloadBytesMin / Max define the uniform range of payload sizes.
	PayloadBytesMin int
	PayloadBytesMax int
}

// RateCurve returns a multiplier applied to EnqueueRate at time t since
// the scenario started. 1.0 = baseline.
type RateCurve func(t time.Duration) float64

// Burst returns a RateCurve that produces a multiplier spike: baseline,
// then multiplier× for burstWindow, then baseline again.
func Burst(burstStart, burstWindow time.Duration, multiplier float64) RateCurve {
	end := burstStart + burstWindow
	return func(t time.Duration) float64 {
		if t >= burstStart && t < end {
			return multiplier
		}
		return 1.0
	}
}

// Generator turns a Spec into a stream of EnqueueRequests. One Generator
// per run; it owns its rng and pre-computed tenant pool.
type Generator struct {
	spec       Spec
	rng        *rand.Rand
	tenants    []uuid.UUID
	totalWeight float64
	errClasses []qpkg.ErrorClass
}

// NewGenerator constructs a generator. Calling Run on it blocks until
// the spec's Duration elapses or ctx is cancelled.
func NewGenerator(spec Spec) *Generator {
	rng := rand.New(rand.NewPCG(spec.Seed, spec.Seed^0xdeadbeef))

	tenants := make([]uuid.UUID, spec.Tenants)
	for i := range tenants {
		tenants[i] = uuid.New()
	}
	totalW := 0.0
	for _, c := range spec.JobMix {
		totalW += c.Weight
	}

	// Stable ordering so sampling is deterministic.
	classes := []qpkg.ErrorClass{qpkg.ErrorRateLimit, qpkg.ErrorTransient, qpkg.ErrorPermanent}

	return &Generator{
		spec:        spec,
		rng:         rng,
		tenants:     tenants,
		totalWeight: totalW,
		errClasses:  classes,
	}
}

// Tenants returns the generator's tenant pool (snapshot; safe to read).
func (g *Generator) Tenants() []uuid.UUID { return g.tenants }

// Run streams generated jobs into out at the Spec's prescribed rate until
// Duration elapses or ctx is cancelled. It closes out on exit.
//
// Back-pressure: if the receiver can't keep up with the configured rate,
// the generator does not queue indefinitely; it skips enqueues (dropped
// jobs are recorded via the dropped callback). Scenarios that want lossy
// behavior pass a non-nil dropped callback; nil means the generator
// blocks until the channel has room.
func (g *Generator) Run(ctx context.Context, out chan<- qpkg.EnqueueRequest, dropped func()) {
	defer close(out)

	baseRate := g.spec.EnqueueRate
	if baseRate <= 0 {
		return
	}
	start := time.Now()

	// Sleep interval at baseline rate. We adjust by the rate curve each
	// tick so spikes are honored without restarting the ticker.
	baseInterval := time.Second / time.Duration(baseRate)

	ticker := time.NewTicker(baseInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if now.Sub(start) >= g.spec.Duration {
				return
			}
			// Apply rate curve: if current multiplier > 1, generate
			// multiple jobs this tick. If < 1, probabilistically skip.
			mult := 1.0
			if g.spec.RateCurve != nil {
				mult = g.spec.RateCurve(now.Sub(start))
			}
			count := int(mult)
			frac := mult - float64(count)
			if frac > 0 && g.rng.Float64() < frac {
				count++
			}
			for i := 0; i < count; i++ {
				req := g.nextRequest()
				if dropped != nil {
					select {
					case out <- req:
					default:
						dropped()
					}
				} else {
					select {
					case <-ctx.Done():
						return
					case out <- req:
					}
				}
			}
		}
	}
}

// nextRequest builds one randomized EnqueueRequest.
func (g *Generator) nextRequest() qpkg.EnqueueRequest {
	return qpkg.EnqueueRequest{
		TenantID: g.pickTenant(),
		Kind:     g.pickKind().Kind,
		Payload:  g.buildPayload(g.pickKind()),
	}
}

// pickTenant samples from the tenant pool respecting TenantSkew.
func (g *Generator) pickTenant() uuid.UUID {
	if len(g.tenants) == 0 {
		return uuid.Nil
	}
	// With probability TenantSkew, return tenant 0 (the "noisy" one).
	if g.spec.TenantSkew > 0 && g.rng.Float64() < g.spec.TenantSkew {
		return g.tenants[0]
	}
	// Otherwise uniform across the rest (or all if there's no skew).
	startIdx := 0
	if g.spec.TenantSkew > 0 && len(g.tenants) > 1 {
		startIdx = 1
	}
	if startIdx >= len(g.tenants) {
		return g.tenants[0]
	}
	return g.tenants[startIdx+g.rng.IntN(len(g.tenants)-startIdx)]
}

// pickKind samples a JobClass weighted by Weight.
func (g *Generator) pickKind() JobClass {
	if len(g.spec.JobMix) == 0 {
		return JobClass{}
	}
	if g.totalWeight <= 0 {
		return g.spec.JobMix[0]
	}
	r := g.rng.Float64() * g.totalWeight
	acc := 0.0
	for _, c := range g.spec.JobMix {
		acc += c.Weight
		if r < acc {
			return c
		}
	}
	return g.spec.JobMix[len(g.spec.JobMix)-1]
}

// buildPayload constructs a synthetic JobPayload for a JobClass, honoring
// ErrorInjection.
func (g *Generator) buildPayload(c JobClass) qpkg.JobPayload {
	var workMs int64
	if c.WorkMillisMax > c.WorkMillisMin {
		workMs = c.WorkMillisMin + g.rng.Int64N(c.WorkMillisMax-c.WorkMillisMin+1)
	} else {
		workMs = c.WorkMillisMin
	}
	var bytesLen int
	if c.PayloadBytesMax > c.PayloadBytesMin {
		bytesLen = c.PayloadBytesMin + g.rng.IntN(c.PayloadBytesMax-c.PayloadBytesMin+1)
	} else {
		bytesLen = c.PayloadBytesMin
	}
	var bytes []byte
	if bytesLen > 0 {
		bytes = make([]byte, bytesLen)
		for i := range bytes {
			bytes[i] = byte(g.rng.IntN(256))
		}
	}
	payload := qpkg.JobPayload{
		WorkMillis: workMs,
		Bytes:      bytes,
	}
	// Error injection: iterate classes in stable order so probabilities
	// compose predictably. If a class is not present in ErrorInjection,
	// it's treated as 0%.
	for _, cls := range g.errClasses {
		p := g.spec.ErrorInjection[cls]
		if p <= 0 {
			continue
		}
		if g.rng.Float64() < p {
			payload.FailWithError = string(cls)
			break
		}
	}
	return payload
}
