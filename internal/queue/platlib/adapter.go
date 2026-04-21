// Package platlib adapts the benchmark Harness contract to the
// rstudio/platform-lib queue (pkg/rsqueue).
//
// Design notes
//
// platform-lib's queue is lower-level than River: workers pull work
// via an Agent loop, maintain permit heartbeats while processing, and
// ack completion by deleting the permit. Work types are uint64-
// discriminated, not string-kind-discriminated. The adapter maps our
// string Kind values to stable uint64 type codes (see kindCode).
//
// This adapter is a skeleton in Phase 2:
//
//   - Connection + migration happens in New.
//   - Register records user WorkerFuncs.
//   - Start launches the worker-loop goroutine pool.
//   - Enqueue calls Push / AddressedPush.
//   - Stop signals workers, waits for drain.
//
// The permit-heartbeat goroutine and worker-loop are sketched here with
// TODO markers noting decisions that still require author (Jon's) review,
// particularly: (a) the permit-heartbeat interval default, (b) how
// priority maps from our Harness contract, and (c) whether we want to
// exercise group-work semantics or leave them as a benchmark scenario.
package platlib

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
)

// Harness implements qpkg.Harness against platform-lib's rsqueue.
type Harness struct {
	cfg    qpkg.Config
	router qpkg.KindToQueue

	pool    *pgxpool.Pool
	mu      sync.RWMutex
	workers map[qpkg.Kind]qpkg.WorkerFunc
	typeOf  map[qpkg.Kind]uint64 // stable type code per Kind
	started bool

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options lets callers override adapter-specific defaults.
//
// TODO(jon): the right defaults for HeartbeatInterval and PermitMaxIdle
// depend on platform-lib's permit expiration semantics. Suggested starting
// points are placeholders; tune based on the author's guidance before
// Phase 3 scenarios run.
type Options struct {
	// Router resolves Kind → queue name. If nil, qpkg.DefaultRouter.
	Router qpkg.KindToQueue

	// HeartbeatInterval controls how often workers extend their permits.
	// Must be shorter than the queue's permit expiration. If zero, a
	// TODO-driven default is used once wiring lands (see TODO below).
	HeartbeatInterval time.Duration

	// DefaultPriority is the priority used for Enqueue calls (platform-lib
	// is priority-aware; our Harness contract is not). Lower = higher
	// priority. 0 is highest.
	DefaultPriority uint64
}

// New returns a platform-lib-backed harness.
//
// TODO(jon): choose the queue implementation. rsqueue has implementations
// under pkg/rsqueue/impls — the Postgres one should align with our
// docker-compose Postgres. Wire the concrete queue.Queue constructor here.
func New(ctx context.Context, cfg qpkg.Config, opts Options) (*Harness, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("platlib adapter: %w", err)
	}
	router := opts.Router
	if router == nil {
		router = qpkg.DefaultRouter
	}

	pool, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("platlib adapter: connect postgres: %w", err)
	}

	// TODO(jon): run platform-lib's schema migrations for rsqueue here.
	// The adapter should be self-contained: calling New on a clean DB
	// leaves the DB ready to accept Enqueue calls. For apples-to-apples
	// measurement with the River adapter (which auto-migrates), this
	// adapter should too.

	return &Harness{
		cfg:     cfg,
		router:  router,
		pool:    pool,
		workers: make(map[qpkg.Kind]qpkg.WorkerFunc),
		typeOf:  make(map[qpkg.Kind]uint64),
		stopCh:  make(chan struct{}),
	}, nil
}

// Register records the user's worker for a Kind and assigns a stable
// uint64 type code for platform-lib's Work discriminator.
func (h *Harness) Register(kind qpkg.Kind, fn qpkg.WorkerFunc) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return errors.New("platlib adapter: Register after Start")
	}
	if _, exists := h.workers[kind]; exists {
		return fmt.Errorf("platlib adapter: Kind %q already registered", kind)
	}
	h.workers[kind] = fn
	h.typeOf[kind] = kindCode(kind)
	return nil
}

// Start launches the worker-loop goroutine pool.
//
// TODO(jon): implement the Agent loop. Each worker goroutine should:
//
//  1. Call queue.Get(ctx, maxPriority, priorityChan, supportedTypes, stopCh)
//     to obtain work. This blocks until work is available or the loop is
//     asked to stop.
//  2. Resolve the WorkerFunc by the work's Type() uint64 → Kind via h.typeOf reverse lookup.
//  3. Launch a heartbeat goroutine that calls queue.Extend(permit) at
//     HeartbeatInterval until the work is done.
//  4. Invoke the WorkerFunc; on success, call queue.Delete(permit); on
//     failure, call queue.RecordFailure(address, err) for addressed work,
//     or requeue for priority-only work.
//  5. Repeat.
func (h *Harness) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return errors.New("platlib adapter: Start called twice")
	}
	// TODO(jon): launch N worker goroutines per queue config.
	h.started = true
	return nil
}

// Enqueue maps to queue.Push for non-addressed work.
//
// TODO(jon): decide whether to use Push or AddressedPush by default.
// AddressedPush gives dedup-by-address which maps well to unique-jobs-per-period
// scenarios (river has UniqueOpts for this). The Harness contract doesn't
// expose uniqueness; benchmark scenarios that want dedup can use library-
// specific options via the constructor's Options struct.
func (h *Harness) Enqueue(ctx context.Context, tenantID uuid.UUID, kind qpkg.Kind, payload qpkg.JobPayload) (qpkg.JobID, error) {
	h.mu.RLock()
	if !h.started {
		h.mu.RUnlock()
		return "", errors.New("platlib adapter: Enqueue before Start")
	}
	code, ok := h.typeOf[kind]
	h.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("platlib adapter: no worker registered for kind %q", kind)
	}
	_ = code     // TODO(jon): build queue.Work with Type() returning code
	_ = tenantID // TODO(jon): carry tenant_id through Work serialization
	_ = payload
	return "", errors.New("platlib adapter: Enqueue not yet implemented")
}

// EnqueueMany is a loop over Enqueue for now. platform-lib's queue
// does not appear to expose a native bulk path (verify with author).
func (h *Harness) EnqueueMany(ctx context.Context, jobs []qpkg.EnqueueRequest) ([]qpkg.JobID, error) {
	out := make([]qpkg.JobID, 0, len(jobs))
	for _, j := range jobs {
		id, err := h.Enqueue(ctx, j.TenantID, j.Kind, j.Payload)
		if err != nil {
			return out, err
		}
		out = append(out, id)
	}
	return out, nil
}

// Stats surfaces what platform-lib exposes via its queue interface.
// TODO(jon): platform-lib's Peek could back this; decide on a cheap query.
func (h *Harness) Stats(ctx context.Context) (qpkg.Stats, error) {
	totalWorkers := 0
	for _, qc := range h.cfg.Queues {
		totalWorkers += qc.MaxWorkers
	}
	return qpkg.Stats{WorkersTotal: totalWorkers}, nil
}

// Stop signals worker loops and waits for drain.
func (h *Harness) Stop(ctx context.Context) error {
	h.mu.Lock()
	if !h.started {
		h.mu.Unlock()
		return nil
	}
	h.started = false
	close(h.stopCh)
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	h.pool.Close()
	return nil
}

// kindCode returns a stable uint64 type code for a Kind. The mapping is
// fnv-1a of the Kind string so ordering is consistent across runs without
// a central registry. platform-lib's queue discriminator is uint64 — the
// actual value isn't semantic, only stable identity matters.
func kindCode(k qpkg.Kind) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(k); i++ {
		h ^= uint64(k[i])
		h *= prime64
	}
	return h
}

// Compile-time check that Harness satisfies qpkg.Harness.
var _ qpkg.Harness = (*Harness)(nil)
