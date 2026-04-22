// Package runner orchestrates one benchmark run: start the harness,
// register workers that record per-job metrics, drive the workload
// generator, sample resources, then tear everything down.
package runner

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonyoder/queue-benchmark/internal/metrics"
	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
	"github.com/jonyoder/queue-benchmark/internal/workload"
)

// Config bundles everything a Run needs.
type Config struct {
	Library  string
	Harness  qpkg.Harness
	Spec     workload.Spec
	Recorder *metrics.Recorder

	// EnqueueConcurrency governs how many goroutines pull from the
	// generator's output channel and issue Enqueue calls in parallel.
	// Default: 4.
	EnqueueConcurrency int

	// DrainTimeout is how long to wait after the generator stops for
	// already-enqueued jobs to finish. Default: Spec.Duration.
	DrainTimeout time.Duration

	// ResourceSampleInterval is how often to sample RSS / goroutine count.
	// Default: 1s.
	ResourceSampleInterval time.Duration
}

// Run executes the scenario end-to-end. It is idempotent with respect to
// the Harness — caller retains ownership and can reuse (but typically
// constructs a fresh harness per Run for clean isolation).
func Run(ctx context.Context, cfg Config) error {
	if cfg.EnqueueConcurrency <= 0 {
		cfg.EnqueueConcurrency = 4
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = cfg.Spec.Duration
	}
	if cfg.ResourceSampleInterval <= 0 {
		cfg.ResourceSampleInterval = time.Second
	}

	rec := cfg.Recorder
	var enqueueCount, completedCount, failedCount atomic.Int64

	// Record run_start.
	if err := rec.Record(metrics.KindRunStart, map[string]any{
		"library":        cfg.Library,
		"scenario":       cfg.Spec.Name,
		"duration_sec":   cfg.Spec.Duration.Seconds(),
		"enqueue_rate":   cfg.Spec.EnqueueRate,
		"tenants":        cfg.Spec.Tenants,
		"tenant_skew":    cfg.Spec.TenantSkew,
		"seed":           cfg.Spec.Seed,
	}); err != nil {
		return fmt.Errorf("record run_start: %w", err)
	}

	// Register one worker per Kind in the mix. Each worker records
	// start/complete events and calls Simulate for synthetic work.
	kinds := make(map[qpkg.Kind]bool)
	for _, c := range cfg.Spec.JobMix {
		kinds[c.Kind] = true
	}
	for k := range kinds {
		kind := k // capture loop variable
		workerFn := func(wctx context.Context, job qpkg.Job) error {
			startedAt := time.Now()
			_ = rec.Record(metrics.KindJobStarted, map[string]any{
				"job_id":    string(job.ID),
				"tenant_id": job.TenantID.String(),
				"kind":      string(job.Kind),
				"attempt":   job.Attempt,
			})
			err := qpkg.Simulate(wctx, job)
			duration := time.Since(startedAt)
			if err != nil {
				failedCount.Add(1)
				class := ""
				if se, ok := qpkg.IsSimulatedError(err); ok {
					class = string(se.Class)
				}
				_ = rec.Record(metrics.KindJobFailed, map[string]any{
					"job_id":        string(job.ID),
					"tenant_id":     job.TenantID.String(),
					"kind":          string(job.Kind),
					"attempt":       job.Attempt,
					"duration_ms":   duration.Milliseconds(),
					"error_class":   class,
					"error_message": err.Error(),
				})
				return err
			}
			completedCount.Add(1)
			_ = rec.Record(metrics.KindJobCompleted, map[string]any{
				"job_id":      string(job.ID),
				"tenant_id":   job.TenantID.String(),
				"kind":        string(job.Kind),
				"attempt":     job.Attempt,
				"duration_ms": duration.Milliseconds(),
			})
			return nil
		}
		if err := cfg.Harness.Register(kind, workerFn); err != nil {
			return fmt.Errorf("register worker %q: %w", kind, err)
		}
	}

	// Start the harness.
	if err := cfg.Harness.Start(ctx); err != nil {
		return fmt.Errorf("start harness: %w", err)
	}

	// Resource sampler: RSS, goroutines, enqueue/complete counters.
	samplerCtx, stopSampler := context.WithCancel(ctx)
	defer stopSampler()
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		t := time.NewTicker(cfg.ResourceSampleInterval)
		defer t.Stop()
		for {
			select {
			case <-samplerCtx.Done():
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				_ = rec.Record(metrics.KindResourceSample, map[string]any{
					"goroutines": runtime.NumGoroutine(),
					"rss_bytes":  m.Sys,
					"heap_bytes": m.HeapAlloc,
					"enqueued":   enqueueCount.Load(),
					"completed":  completedCount.Load(),
					"failed":     failedCount.Load(),
				})
			}
		}
	}()

	// Generator produces EnqueueRequests into reqs; workers pull from reqs
	// and issue Enqueue in parallel.
	reqs := make(chan qpkg.EnqueueRequest, cfg.Spec.EnqueueRate*2)
	gen := workload.NewGenerator(cfg.Spec)
	genCtx, stopGen := context.WithCancel(ctx)
	defer stopGen()

	genDone := make(chan struct{})
	go func() {
		defer close(genDone)
		gen.Run(genCtx, reqs, nil)
	}()

	// Enqueue workers.
	var enqueueWG sync.WaitGroup
	for i := 0; i < cfg.EnqueueConcurrency; i++ {
		enqueueWG.Add(1)
		go func() {
			defer enqueueWG.Done()
			for req := range reqs {
				enqueuedAt := time.Now()
				jobID, err := cfg.Harness.Enqueue(ctx, req.TenantID, req.Kind, req.Payload)
				if err != nil {
					_ = rec.Record(metrics.KindJobFailed, map[string]any{
						"tenant_id":   req.TenantID.String(),
						"kind":        string(req.Kind),
						"stage":       "enqueue",
						"error_message": err.Error(),
					})
					continue
				}
				enqueueCount.Add(1)
				_ = rec.Record(metrics.KindJobEnqueued, map[string]any{
					"job_id":     string(jobID),
					"tenant_id":  req.TenantID.String(),
					"kind":       string(req.Kind),
					"enqueue_ms": time.Since(enqueuedAt).Milliseconds(),
				})
			}
		}()
	}

	// Wait for the generator to finish (duration elapsed) or ctx cancel.
	select {
	case <-genDone:
	case <-ctx.Done():
		stopGen()
		<-genDone
	}

	// Generator closed reqs when it returned; enqueue workers will
	// drain and exit.
	enqueueWG.Wait()

	// Drain: allow already-enqueued jobs to complete.
	drainCtx, drainCancel := context.WithTimeout(ctx, cfg.DrainTimeout)
	defer drainCancel()
	<-drainCtx.Done()

	// Stop the harness and sampler.
	stopSampler()
	samplerWG.Wait()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	if err := cfg.Harness.Stop(stopCtx); err != nil {
		_ = rec.Record(metrics.KindRunEnd, map[string]any{
			"stop_error": err.Error(),
		})
		return fmt.Errorf("stop harness: %w", err)
	}

	if err := rec.Record(metrics.KindRunEnd, map[string]any{
		"enqueued":  enqueueCount.Load(),
		"completed": completedCount.Load(),
		"failed":    failedCount.Load(),
	}); err != nil {
		return fmt.Errorf("record run_end: %w", err)
	}
	return nil
}
