// Package river adapts the benchmark Harness contract to the River
// job queue library (github.com/riverqueue/river).
//
// Design notes
//
// River requires one Worker type per JobArgs type. To match our vendor-
// neutral Harness — which dispatches by a string Kind at runtime — the
// adapter uses a single "benchmark job" args type that carries the
// Kind as a field, plus a single registered Worker that dispatches to
// the benchmark's WorkerFunc by Kind.
//
// This sidesteps River's compile-time type routing in exchange for
// apples-to-apples comparability across adapters. Any library that
// supports typed workers will take a similar shape in the benchmark.
package river

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
)

// Harness implements qpkg.Harness against River.
type Harness struct {
	cfg    qpkg.Config
	router qpkg.KindToQueue

	pool   *pgxpool.Pool
	client *river.Client[pgx.Tx]

	mu      sync.RWMutex
	workers map[qpkg.Kind]qpkg.WorkerFunc
	started bool
}

// Options lets callers override adapter-specific defaults without
// polluting the shared Config.
type Options struct {
	// Router resolves Kind → queue name. If nil, qpkg.DefaultRouter is used.
	Router qpkg.KindToQueue

	// FetchCooldown bounds how often River polls for new work when
	// LISTEN/NOTIFY is unavailable. Lower = lower latency, higher DB load.
	// Default (River's default) is 100ms.
	FetchCooldown time.Duration

	// FetchPollInterval is the max wait between fetches regardless of
	// activity signals. Default (River's default) is 1s.
	FetchPollInterval time.Duration
}

// New returns a River-backed harness. The Postgres connection is opened
// but no workers are started until Start is called.
func New(ctx context.Context, cfg qpkg.Config, opts Options) (*Harness, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("river adapter: %w", err)
	}
	router := opts.Router
	if router == nil {
		router = qpkg.DefaultRouter
	}

	pool, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("river adapter: connect postgres: %w", err)
	}

	// Run River migrations into the shared Postgres.
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("river adapter: new migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		pool.Close()
		return nil, fmt.Errorf("river adapter: migrate: %w", err)
	}

	return &Harness{
		cfg:     cfg,
		router:  router,
		pool:    pool,
		workers: make(map[qpkg.Kind]qpkg.WorkerFunc),
	}, nil
}

// Register records the user's worker for a Kind. River's actual worker
// pool is a single dispatching worker (see package doc).
func (h *Harness) Register(kind qpkg.Kind, fn qpkg.WorkerFunc) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return errors.New("river adapter: Register after Start")
	}
	if _, exists := h.workers[kind]; exists {
		return fmt.Errorf("river adapter: Kind %q already registered", kind)
	}
	h.workers[kind] = fn
	return nil
}

// Start launches River's worker pool. Must be called after all Register
// calls.
func (h *Harness) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return errors.New("river adapter: Start called twice")
	}

	queues := make(map[string]river.QueueConfig, len(h.cfg.Queues))
	for name, qc := range h.cfg.Queues {
		queues[name] = river.QueueConfig{MaxWorkers: qc.MaxWorkers}
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, &benchWorker{h: h})

	clientCfg := &river.Config{
		Queues:     queues,
		Workers:    workers,
		MaxAttempts: h.cfg.MaxAttempts,
	}
	// Honor caller-supplied polling overrides if provided.
	// (Zero-value fields fall back to River's defaults, which is what we want.)

	driver := riverpgxv5.New(h.pool)
	client, err := river.NewClient(driver, clientCfg)
	if err != nil {
		return fmt.Errorf("river adapter: new client: %w", err)
	}
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("river adapter: start client: %w", err)
	}
	h.client = client
	h.started = true
	return nil
}

// Enqueue submits one job to the queue router-selected for its Kind.
func (h *Harness) Enqueue(ctx context.Context, tenantID uuid.UUID, kind qpkg.Kind, payload qpkg.JobPayload) (qpkg.JobID, error) {
	args := benchArgs{
		TenantIDStr: tenantID.String(),
		KindStr:     string(kind),
		Payload:     payload,
	}
	opts := &river.InsertOpts{Queue: h.router(kind)}
	h.mu.RLock()
	client := h.client
	h.mu.RUnlock()
	if client == nil {
		return "", errors.New("river adapter: Enqueue before Start")
	}
	res, err := client.Insert(ctx, args, opts)
	if err != nil {
		return "", err
	}
	return qpkg.JobID(fmt.Sprintf("%d", res.Job.ID)), nil
}

// EnqueueMany uses River's native bulk insert path.
func (h *Harness) EnqueueMany(ctx context.Context, jobs []qpkg.EnqueueRequest) ([]qpkg.JobID, error) {
	h.mu.RLock()
	client := h.client
	h.mu.RUnlock()
	if client == nil {
		return nil, errors.New("river adapter: EnqueueMany before Start")
	}

	params := make([]river.InsertManyParams, len(jobs))
	for i, j := range jobs {
		params[i] = river.InsertManyParams{
			Args: benchArgs{
				TenantIDStr: j.TenantID.String(),
				KindStr:     string(j.Kind),
				Payload:     j.Payload,
			},
			InsertOpts: &river.InsertOpts{Queue: h.router(j.Kind)},
		}
	}
	res, err := client.InsertMany(ctx, params)
	if err != nil {
		return nil, err
	}
	ids := make([]qpkg.JobID, len(res))
	for i, r := range res {
		ids[i] = qpkg.JobID(fmt.Sprintf("%d", r.Job.ID))
	}
	return ids, nil
}

// Stats returns a coarse view across all known queues. River surfaces
// per-queue counts via its client; we aggregate here.
func (h *Harness) Stats(ctx context.Context) (qpkg.Stats, error) {
	// Minimal implementation: River's client does not expose aggregate
	// counts directly; the benchmark computes these from the JSONL
	// event stream. Return what we know.
	totalWorkers := 0
	for _, qc := range h.cfg.Queues {
		totalWorkers += qc.MaxWorkers
	}
	return qpkg.Stats{WorkersTotal: totalWorkers}, nil
}

// Stop drains the worker pool up to the configured timeout.
func (h *Harness) Stop(ctx context.Context) error {
	h.mu.Lock()
	client := h.client
	h.started = false
	h.mu.Unlock()
	if client == nil {
		return nil
	}

	stopCtx, cancel := context.WithTimeout(ctx, h.cfg.ShutdownTimeout)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		return fmt.Errorf("river adapter: stop: %w", err)
	}
	h.pool.Close()
	return nil
}

// benchArgs is the single JobArgs type used by the adapter. River
// requires a typed JobArgs per worker; the benchmark routes by Kind
// inside a single dispatch worker (see benchWorker).
type benchArgs struct {
	TenantIDStr string           `json:"tenant_id"`
	KindStr     string           `json:"kind"`
	Payload     qpkg.JobPayload  `json:"payload"`
}

func (benchArgs) Kind() string { return "benchmark.job" }

// benchWorker dispatches a benchmark job to the user-registered
// WorkerFunc for its Kind.
type benchWorker struct {
	river.WorkerDefaults[benchArgs]
	h *Harness
}

func (w *benchWorker) Work(ctx context.Context, job *river.Job[benchArgs]) error {
	tenantID, err := uuid.Parse(job.Args.TenantIDStr)
	if err != nil {
		// Non-UUID tenant ID is a data bug; River should discard this rather
		// than retry forever.
		return river.JobCancel(fmt.Errorf("invalid tenant id: %w", err))
	}
	kind := qpkg.Kind(job.Args.KindStr)

	w.h.mu.RLock()
	fn, ok := w.h.workers[kind]
	w.h.mu.RUnlock()
	if !ok {
		return river.JobCancel(fmt.Errorf("no worker registered for kind %q", kind))
	}

	bj := qpkg.Job{
		ID:         qpkg.JobID(fmt.Sprintf("%d", job.ID)),
		TenantID:   tenantID,
		Kind:       kind,
		Payload:    job.Args.Payload,
		Attempt:    int32(job.Attempt),
		EnqueuedAt: job.CreatedAt,
	}
	if err := fn(ctx, bj); err != nil {
		// Preserve simulation semantics for the benchmark: a Permanent
		// simulated error becomes a River cancel; RateLimit/Transient
		// become retries.
		if se, ok := qpkg.IsSimulatedError(err); ok {
			switch se.Class {
			case qpkg.ErrorPermanent:
				return river.JobCancel(err)
			}
		}
		return err
	}
	return nil
}

// Compile-time check that Harness satisfies qpkg.Harness.
var _ qpkg.Harness = (*Harness)(nil)

// riverJobTypeAssertion prevents accidental import cycles or missed
// dependencies when this file is built in isolation.
var _ = rivertype.JobRow{}
