// Package queue defines the vendor-neutral contract implemented by each
// library adapter under evaluation. The benchmark harness drives its
// workload through this interface; adapters translate to their library's
// native API.
//
// The contract intentionally constrains capabilities to what every Postgres-
// backed job queue can reasonably offer. Adapter-specific knobs (e.g.,
// unique jobs, scheduling semantics) are exposed through per-library
// Options structs accessible via the Harness constructor, not this
// interface.
package queue

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Harness is the primary contract a library adapter implements. Each
// adapter produces a Harness by wrapping its native queue client and
// worker pool in a common API.
//
// A Harness owns its worker pool and its connection to Postgres. The
// benchmark runner creates one Harness per scenario run, drives it with
// a workload, and tears it down.
type Harness interface {
	// Start spins up the worker pool. Workers are registered via Register
	// before Start is called.
	Start(ctx context.Context) error

	// Enqueue submits a single job. TenantID distinguishes jobs for per-
	// tenant fairness measurements. Kind selects the worker that will
	// handle this job.
	Enqueue(ctx context.Context, tenantID uuid.UUID, kind Kind, payload JobPayload) (JobID, error)

	// EnqueueMany submits a batch of jobs. Adapters SHOULD use their
	// library's most efficient bulk-insert path if one exists.
	EnqueueMany(ctx context.Context, jobs []EnqueueRequest) ([]JobID, error)

	// Register associates a worker with a job kind. Must be called
	// before Start. Calling Register after Start is implementation-
	// defined and should be avoided by the harness.
	Register(kind Kind, worker WorkerFunc) error

	// Stop drains the worker pool. Behavior under in-flight jobs is
	// implementation-defined but SHOULD attempt graceful shutdown up
	// to the timeout specified at construction time.
	Stop(ctx context.Context) error

	// Stats returns a point-in-time snapshot of queue state. Used for
	// live metrics; values are best-effort and may be slightly stale.
	Stats(ctx context.Context) (Stats, error)
}

// JobID uniquely identifies a job within a single benchmark run. Adapters
// may assign these values from their library's native ID type; they are
// opaque to the harness.
type JobID string

// Kind identifies a worker class. Examples in this benchmark include
// "document_process", "entity_update", "daily_coordinator". Adapters
// register one WorkerFunc per Kind.
type Kind string

// JobPayload is the data passed to a worker. Benchmarks use a synthetic
// payload shape describing how much work the worker should do; no
// domain-specific content is present.
type JobPayload struct {
	// WorkMillis is the target wall-clock duration the worker function
	// will sleep / simulate before returning. Used to model job-duration
	// distributions.
	WorkMillis int64 `json:"work_millis"`

	// Bytes is the payload size. Included to surface serialization cost
	// differences between adapters.
	Bytes []byte `json:"bytes"`

	// FailWithError, if non-empty, causes the worker to return the
	// specified error class (e.g., "rate_limit", "transient", "permanent")
	// to exercise retry and error-classification behavior.
	FailWithError string `json:"fail_with_error,omitempty"`

	// Attempt records which attempt this is. Set by the harness, not the
	// caller.
	Attempt int32 `json:"attempt,omitempty"`
}

// EnqueueRequest describes one job for the bulk enqueue path.
type EnqueueRequest struct {
	TenantID uuid.UUID
	Kind     Kind
	Payload  JobPayload
}

// WorkerFunc is the implementation a library adapter invokes for each
// job assigned to it.
//
// The adapter is responsible for translating this contract to its
// library's native worker signature. In particular:
//
//   - An error return SHOULD be classified by the adapter as retryable
//     vs. permanent, according to the adapter's Options; the default
//     is "retryable up to MaxAttempts".
//   - Context cancellation SHOULD be propagated to the library so it
//     can cancel the job if supported.
type WorkerFunc func(ctx context.Context, job Job) error

// Job is what a WorkerFunc receives. It carries the benchmark-relevant
// fields plus enough metadata for the worker to report outcomes.
type Job struct {
	ID        JobID
	TenantID  uuid.UUID
	Kind      Kind
	Payload   JobPayload
	Attempt   int32
	EnqueuedAt time.Time
}

// Stats is a point-in-time view of queue health. Adapters fill in what
// their library exposes; zero values mean "not available".
type Stats struct {
	Pending    int64 // jobs available to run
	Running    int64 // jobs currently being processed
	Completed  int64 // jobs finished successfully since Start
	Failed     int64 // jobs that failed (will retry)
	Discarded  int64 // jobs that exceeded retry budget
	WorkersTotal int
	WorkersBusy  int
}

// ErrorClass is a synthetic error classifier used by the benchmark worker
// to simulate common failure modes. Adapters SHOULD respect the
// semantics when configuring retry policies:
//
//   - RateLimit: caller simulated a provider rate-limit (HTTP 429).
//     Adapters should back off longer than the default.
//   - Transient: a recoverable error; default retry applies.
//   - Permanent: not recoverable; adapter should cancel without retry.
type ErrorClass string

const (
	ErrorRateLimit ErrorClass = "rate_limit"
	ErrorTransient ErrorClass = "transient"
	ErrorPermanent ErrorClass = "permanent"
)
