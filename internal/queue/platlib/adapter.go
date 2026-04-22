// Package platlib adapts the benchmark Harness contract to the
// rstudio/platform-lib queue (pkg/rsqueue).
//
// Architecture overview
//
// Unlike River, rsqueue is a set of primitives rather than a monolithic
// client. The adapter composes them as follows:
//
//	pgxpool (DB)
//	  └── pgStore (implements queue.QueueStore)
//	        └── DatabaseQueue (implements queue.Queue)
//	              └── DefaultAgent (worker loop, heartbeat, dispatch)
//	                    └── RunnerFactory (dispatch by work type)
//	                          └── benchRunner (our one runner, routes by Kind)
//
// Notifications flow through local.ListenerProvider → broadcaster →
// three typed channels consumed by DatabaseQueue's internal broadcaster.
// The adapter is single-process; cross-process notifications would
// require swapping the local listener for the pgx one.
package platlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rstudio/platform-lib/v3/pkg/rsnotify/broadcaster"
	"github.com/rstudio/platform-lib/v3/pkg/rsnotify/listener"
	"github.com/rstudio/platform-lib/v3/pkg/rsnotify/listeners/local"
	"github.com/rstudio/platform-lib/v3/pkg/rsqueue/agent"
	agenttypes "github.com/rstudio/platform-lib/v3/pkg/rsqueue/agent/types"
	"github.com/rstudio/platform-lib/v3/pkg/rsqueue/impls/database"
	"github.com/rstudio/platform-lib/v3/pkg/rsqueue/metrics"
	plqueue "github.com/rstudio/platform-lib/v3/pkg/rsqueue/queue"
	"github.com/rstudio/platform-lib/v3/pkg/rsqueue/runnerfactory"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
)

// Harness implements qpkg.Harness against rsqueue.
type Harness struct {
	cfg  qpkg.Config
	opts Options

	pool    *pgxpool.Pool
	store   *pgStore
	queue   plqueue.Queue
	agent   *agent.DefaultAgent
	factory *runnerfactory.RunnerFactory

	// Notification plumbing.
	lp            *local.ListenerProvider
	lf            *local.ListenerFactory
	broadcaster   broadcaster.Broadcaster
	bListener     listener.Listener
	stopBroadcast chan bool
	stopQueue     chan bool

	mu      sync.RWMutex
	workers map[qpkg.Kind]qpkg.WorkerFunc
	started bool

	// Background retry goroutines. stopCh closes on Stop; wg tracks
	// in-flight BackoffFn goroutines so Stop can wait for them.
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options lets callers override adapter-specific defaults.
type Options struct {
	// DefaultPriority is the priority used for Enqueue calls (rsqueue is
	// priority-aware; our Harness contract is not). Lower = higher
	// priority. 0 is highest. Default: 10 (arbitrary middle ground).
	DefaultPriority uint64

	// UseAddressedPush, if true, uses AddressedPush for every enqueue with
	// a synthetic unique address ("bench:{uuid}"). Default is false (plain
	// Push) to mirror River's behavior. Scenarios that want dedup semantics
	// can flip this.
	UseAddressedPush bool

	// BackoffFn returns the delay to wait before re-enqueuing a failed
	// job. Called with the upcoming attempt number (2 for first retry, 3
	// for second, etc.). If nil or returns 0, retries are immediate —
	// the worker that handled the failing attempt returns, the job is
	// re-enqueued synchronously, and the next available worker picks it
	// up at LISTEN/NOTIFY speed.
	//
	// A non-zero duration schedules the re-enqueue on a background
	// goroutine so the worker slot is released immediately. The
	// goroutine respects the harness shutdown signal.
	//
	// ExponentialBackoffRiverLike mirrors River's default attempt^4
	// policy for apples-to-apples comparison of retry shapes.
	BackoffFn func(nextAttempt int32) time.Duration
}

// ExponentialBackoffRiverLike mirrors River's default retry policy,
// which spaces retries by `failed_attempt^4` seconds: after attempt 1
// fails wait 1s, after attempt 2 fails wait 16s, after attempt 3 fails
// wait 81s, etc.
//
// Argument is the attempt about to be scheduled (i.e., 2 for the first
// retry, 3 for the second). The failed attempt is therefore
// `nextAttempt - 1`. Useful as a BackoffFn for apples-to-apples retry-
// shape comparisons with River.
func ExponentialBackoffRiverLike(nextAttempt int32) time.Duration {
	failedAttempt := int64(nextAttempt - 1)
	if failedAttempt < 1 {
		failedAttempt = 1
	}
	// failedAttempt=1 → 1s, 2 → 16s, 3 → 81s (matches River defaults).
	return time.Duration(failedAttempt*failedAttempt*failedAttempt*failedAttempt) * time.Second
}

// New returns an rsqueue-backed harness. Postgres connection is opened,
// schema is applied, notification plumbing is wired, and DatabaseQueue is
// constructed. No workers run until Start is called.
func New(ctx context.Context, cfg qpkg.Config, opts Options) (*Harness, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("platlib adapter: %w", err)
	}
	if opts.DefaultPriority == 0 {
		opts.DefaultPriority = 10
	}

	pool, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("platlib adapter: connect postgres: %w", err)
	}
	if err := applySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("platlib adapter: %w", err)
	}

	// Local listener + factory + broadcaster for in-process NOTIFY.
	lp := local.NewListenerProvider(local.ListenerProviderArgs{})
	lf := local.NewListenerFactory(lp)

	matcher := listener.NewMatcher("NotifyType")
	matcher.Register(notifyTypeQueue, &queueDbNote{})
	matcher.Register(notifyTypeWorkComplete, &agenttypes.WorkCompleteNotification{})
	matcher.Register(notifyTypeChunk, &chunkNote{})

	bl := lf.New(channelMessages, matcher)
	stopBroadcast := make(chan bool)
	b, err := broadcaster.NewNotificationBroadcaster(bl, stopBroadcast)
	if err != nil {
		pool.Close()
		bl.Stop()
		lf.Shutdown()
		return nil, fmt.Errorf("platlib adapter: new broadcaster: %w", err)
	}

	queueMsgs := b.Subscribe(notifyTypeQueue)
	workMsgs := b.Subscribe(notifyTypeWorkComplete)
	chunkMsgs := b.Subscribe(notifyTypeChunk)

	// Store is notifier-aware so its push operations fan out in-process
	// without needing Postgres NOTIFY round-trips.
	store := newPgStore(pool, localNotifier{lp: lp})

	stopQueue := make(chan bool)
	q, err := database.NewDatabaseQueue(database.DatabaseQueueConfig{
		QueueName:              benchQueueName,
		NotifyTypeWorkReady:    notifyTypeQueue,
		NotifyTypeWorkComplete: notifyTypeWorkComplete,
		NotifyTypeChunk:        notifyTypeChunk,
		ChunkMatcher:           noChunkMatcher{},
		CarrierFactory:         &metrics.EmptyCarrierFactory{},
		QueueStore:             store,
		QueueMsgsChan:          queueMsgs,
		WorkMsgsChan:           workMsgs,
		ChunkMsgsChan:          chunkMsgs,
		StopChan:               stopQueue,
		JobLifecycleWrapper:    &metrics.EmptyJobLifecycleWrapper{},
	})
	if err != nil {
		pool.Close()
		bl.Stop()
		lf.Shutdown()
		return nil, fmt.Errorf("platlib adapter: new queue: %w", err)
	}

	h := &Harness{
		cfg:           cfg,
		opts:          opts,
		pool:          pool,
		store:         store,
		queue:         q,
		lp:            lp,
		lf:            lf,
		broadcaster:   b,
		bListener:     bl,
		stopBroadcast: stopBroadcast,
		stopQueue:     stopQueue,
		workers:       make(map[qpkg.Kind]qpkg.WorkerFunc),
		stopCh:        make(chan struct{}),
	}
	return h, nil
}

// Register records a user worker for a Kind.
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
	return nil
}

// Start wires the RunnerFactory + Agent and launches the worker loop.
func (h *Harness) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return errors.New("platlib adapter: Start called twice")
	}
	h.mu.Unlock()

	supported := &plqueue.DefaultQueueSupportedTypes{}
	factory := runnerfactory.NewRunnerFactory(runnerfactory.RunnerFactoryConfig{
		SupportedTypes: supported,
	})
	// Register one runner per Kind. The RunnerFactory.Add call also flags
	// the work type as enabled on SupportedTypes.
	h.mu.RLock()
	for kind := range h.workers {
		wt := kindCode(kind)
		factory.Add(wt, &benchRunner{h: h, kind: kind})
	}
	h.mu.RUnlock()
	h.factory = factory

	// Concurrency enforcer: total worker slots = sum of cfg.Queues.
	// rsqueue's Concurrencies is priority-aware — callers pass a per-
	// priority concurrency budget. We budget at priority 0 (the default
	// we enqueue at) and fall back to the same value for higher priorities.
	totalWorkers := 0
	for _, qc := range h.cfg.Queues {
		totalWorkers += qc.MaxWorkers
	}
	pri := int64(h.opts.DefaultPriority)
	defaults := map[int64]int64{pri: int64(totalWorkers)}
	enforcer, err := agent.Concurrencies(defaults, nil, []int64{pri})
	if err != nil {
		return fmt.Errorf("platlib adapter: concurrencies: %w", err)
	}

	ag := agent.NewAgent(agent.AgentConfig{
		WorkRunner:             factory,
		Queue:                  h.queue,
		ConcurrencyEnforcer:    enforcer,
		SupportedTypes:         supported,
		NotifyTypeWorkComplete: notifyTypeWorkComplete,
		JobLifecycleWrapper:    &metrics.EmptyJobLifecycleWrapper{},
	})
	h.agent = ag

	// Agent Run callback: relay WorkComplete notifications through our
	// local provider so PollAddress (subscribed on the broadcaster) sees
	// them. Without this, addressed-work pollers fall back to the 5s poll.
	notifyFn := func(n listener.Notification) {
		h.lp.Notify(channelMessages, n)
	}

	h.mu.Lock()
	h.started = true
	h.mu.Unlock()

	// Run the agent loop in its own goroutine; shutdown happens via Stop.
	go func() {
		ag.Run(ctx, notifyFn)
	}()
	return nil
}

// Enqueue pushes one job to the rsqueue.
func (h *Harness) Enqueue(ctx context.Context, tenantID uuid.UUID, kind qpkg.Kind, payload qpkg.JobPayload) (qpkg.JobID, error) {
	h.mu.RLock()
	started := h.started
	_, known := h.workers[kind]
	h.mu.RUnlock()
	if !started {
		return "", errors.New("platlib adapter: Enqueue before Start")
	}
	if !known {
		return "", fmt.Errorf("platlib adapter: no worker registered for kind %q", kind)
	}

	workID := nextWorkID()
	work := benchWork{
		WorkID:      workID,
		TenantIDStr: tenantID.String(),
		KindStr:     string(kind),
		Attempt:     1,
		Payload:     payload,
	}

	if h.opts.UseAddressedPush {
		addr := fmt.Sprintf("bench:%d", workID)
		if err := h.queue.AddressedPush(ctx, h.opts.DefaultPriority, 0, addr, work); err != nil {
			return "", err
		}
		return qpkg.JobID(addr), nil
	}
	if err := h.queue.Push(ctx, h.opts.DefaultPriority, 0, work); err != nil {
		return "", err
	}
	return qpkg.JobID(fmt.Sprintf("bench:%d", workID)), nil
}

// EnqueueMany is currently a loop — rsqueue's Queue interface doesn't
// expose a native bulk path. The loop still benefits from our store's
// post-commit-notify batching when a single Harness.EnqueueMany caller
// wraps its loop in one transaction; that's not done here, so each push
// is its own tx (consistent with the River adapter's behavior).
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

// Stats returns a point-in-time view. rsqueue does not surface aggregate
// counts cheaply; Peek reads unclaimed rows.
func (h *Harness) Stats(ctx context.Context) (qpkg.Stats, error) {
	totalWorkers := 0
	for _, qc := range h.cfg.Queues {
		totalWorkers += qc.MaxWorkers
	}
	return qpkg.Stats{WorkersTotal: totalWorkers}, nil
}

// Stop drains the agent, broadcaster, listener factory, and DB pool.
func (h *Harness) Stop(ctx context.Context) error {
	h.mu.Lock()
	if !h.started {
		h.mu.Unlock()
		h.pool.Close()
		return nil
	}
	h.started = false
	ag := h.agent
	h.mu.Unlock()

	// Signal any in-flight BackoffFn goroutines to abandon their retry.
	close(h.stopCh)
	h.wg.Wait()

	if ag != nil {
		if err := ag.Stop(h.cfg.ShutdownTimeout); err != nil && !errors.Is(err, agent.ErrAgentStopTimeout) {
			return fmt.Errorf("platlib adapter: stop agent: %w", err)
		}
	}
	if h.factory != nil {
		_ = h.factory.Stop(h.cfg.ShutdownTimeout)
	}
	// Signal the DatabaseQueue broadcaster to stop.
	select {
	case h.stopQueue <- true:
	default:
	}
	// Signal the notification broadcaster to stop.
	select {
	case h.stopBroadcast <- true:
	default:
	}
	if h.lf != nil {
		h.lf.Shutdown()
	}
	h.pool.Close()
	return nil
}

// --- supporting types ---

// benchWork is the Work payload carried through rsqueue. Kind and TenantID
// travel inside it because rsqueue doesn't have first-class tenant scope.
// Attempt tracks the retry count; the runner increments it on re-enqueue.
type benchWork struct {
	WorkID      uint64          `json:"work_id"`
	TenantIDStr string          `json:"tenant_id"`
	KindStr     string          `json:"kind"`
	Attempt     int32           `json:"attempt"`
	Payload     qpkg.JobPayload `json:"payload"`
}

// Type satisfies rsqueue's Work interface. We key the worker type on
// the Kind via a stable fnv-1a hash (see kindCode). The returned value
// is resolved by the RunnerFactory at dispatch time.
func (w benchWork) Type() uint64 { return kindCode(qpkg.Kind(w.KindStr)) }

// Address satisfies rsqueue.AddressableWork when UseAddressedPush is true.
func (w benchWork) Address() string { return fmt.Sprintf("bench:%d", w.WorkID) }

// Dir is required by AddressableWork; unused in this benchmark.
func (w benchWork) Dir() string { return "" }

// benchRunner dispatches a benchmark job to the user-registered WorkerFunc
// for its Kind.
type benchRunner struct {
	plqueue.BaseRunner
	h    *Harness
	kind qpkg.Kind
}

func (r *benchRunner) Run(ctx context.Context, work plqueue.RecursableWork) error {
	var bw benchWork
	if err := json.Unmarshal(work.Work, &bw); err != nil {
		return fmt.Errorf("platlib runner: unmarshal: %w", err)
	}
	tenantID, err := uuid.Parse(bw.TenantIDStr)
	if err != nil {
		return fmt.Errorf("platlib runner: tenant id: %w", err)
	}
	kind := qpkg.Kind(bw.KindStr)

	r.h.mu.RLock()
	fn, ok := r.h.workers[kind]
	r.h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("platlib runner: no worker registered for kind %q", kind)
	}
	bj := qpkg.Job{
		ID:         qpkg.JobID(bw.Address()),
		TenantID:   tenantID,
		Kind:       kind,
		Payload:    bw.Payload,
		Attempt:    bw.Attempt,
		EnqueuedAt: time.Now(), // best-effort; rsqueue doesn't plumb Work.CreatedAt through RecursableWork
	}
	workerErr := fn(ctx, bj)
	if workerErr == nil {
		return nil
	}

	// Retry policy
	// -------------
	// rsqueue does not have a built-in retry mechanism — the agent removes
	// the work row after the runner returns, regardless of error. To mirror
	// River's default retry-up-to-MaxAttempts behavior, the runner re-
	// enqueues the job on recoverable errors and returns nil to the agent
	// so the current permit is released cleanly.
	//
	// Permanent simulated errors (benchmark's FailWithError=permanent) do
	// not retry. Other error classes retry until Attempt == MaxAttempts.
	//
	// Caveat: rsqueue's Push has no scheduled-at / delay semantics, so
	// retries are immediate — unlike River's exponential backoff. This
	// creates a DIFFERENT retry shape between libraries: platform-lib
	// re-enqueues quickly against the next available worker; River stages
	// retries with 1s/16s/81s spacing.
	if se, simulated := qpkg.IsSimulatedError(workerErr); simulated && se.Class == qpkg.ErrorPermanent {
		return workerErr // no retry — agent records failure
	}
	maxAttempts := int32(r.h.cfg.MaxAttempts)
	if bw.Attempt >= maxAttempts {
		return workerErr // exhausted — agent records final failure
	}
	// Re-enqueue with incremented attempt. The same benchWork.WorkID means
	// subsequent events still correlate by job_id.
	bw.Attempt++

	// Backoff: if configured, delay re-enqueue on a background goroutine
	// so the worker slot is freed immediately.
	if r.h.opts.BackoffFn != nil {
		if delay := r.h.opts.BackoffFn(bw.Attempt); delay > 0 {
			r.h.wg.Add(1)
			go func(bwCopy benchWork, d time.Duration) {
				defer r.h.wg.Done()
				t := time.NewTimer(d)
				defer t.Stop()
				select {
				case <-t.C:
					// Use a detached context — the harness is still up
					// (Stop waits for wg) so Push will succeed.
					_ = r.h.queue.Push(context.Background(), r.h.opts.DefaultPriority, 0, bwCopy)
				case <-r.h.stopCh:
					// Harness shutting down — drop the retry.
					return
				}
			}(bw, delay)
			return nil
		}
	}

	// Immediate re-enqueue (no backoff).
	if err := r.h.queue.Push(ctx, r.h.opts.DefaultPriority, 0, bw); err != nil {
		return fmt.Errorf("platlib runner: re-enqueue attempt %d: %w (original: %v)", bw.Attempt, err, workerErr)
	}
	return nil
}

// chunkNote is a placeholder wire type registered so the broadcaster
// matcher can route (empty) chunk notifications. The benchmark doesn't
// use chunked storage.
type chunkNote struct {
	NotifyType uint8 `json:"NotifyType"`
}

func (c chunkNote) Type() uint8 { return c.NotifyType }
func (chunkNote) Guid() string  { return "" }

// noChunkMatcher satisfies queue.DatabaseQueueChunkMatcher with a never-match.
type noChunkMatcher struct{}

func (noChunkMatcher) Match(_ listener.Notification, _ string) bool { return false }

// localNotifier adapts a local.ListenerProvider to our store's pgNotifier
// interface. ListenerProvider.Notify doesn't return an error; we wrap
// it to satisfy the store's typed contract.
type localNotifier struct {
	lp *local.ListenerProvider
}

func (l localNotifier) Notify(channel string, n any) error {
	l.lp.Notify(channel, n)
	return nil
}

// kindCode returns a stable uint64 type code for a Kind via fnv-1a.
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

// nextWorkID yields a monotonic id for synthetic addresses.
var workIDCounter atomic.Uint64

func nextWorkID() uint64 { return workIDCounter.Add(1) }

// Compile-time interface check.
var _ qpkg.Harness = (*Harness)(nil)
