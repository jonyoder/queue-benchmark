package queue

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Config is the adapter-agnostic configuration every Harness constructor
// accepts. Per-library knobs that don't fit here belong on library-specific
// Options structs.
type Config struct {
	// PostgresURL is the connection URL for the shared benchmark database.
	PostgresURL string

	// Queues defines the worker pools. Each entry spins up a pool of the
	// specified size processing jobs dispatched to that queue.
	Queues map[string]QueueConfig

	// MaxAttempts sets the default retry budget for jobs. Individual
	// Kinds may override via RetryPolicy (future).
	MaxAttempts int

	// ShutdownTimeout caps how long Stop waits for in-flight jobs to
	// complete before forcing teardown.
	ShutdownTimeout time.Duration
}

// QueueConfig describes one worker pool within the harness.
type QueueConfig struct {
	// MaxWorkers is the concurrent in-flight job cap for this queue.
	MaxWorkers int
}

// Validate returns an error if the config has obviously-wrong values.
// Adapters should call this from their constructor.
func (c Config) Validate() error {
	if c.PostgresURL == "" {
		return errors.New("Config.PostgresURL is required")
	}
	if len(c.Queues) == 0 {
		return errors.New("Config.Queues must specify at least one queue")
	}
	for name, qc := range c.Queues {
		if qc.MaxWorkers <= 0 {
			return fmt.Errorf("Config.Queues[%q].MaxWorkers must be > 0", name)
		}
	}
	if c.MaxAttempts < 1 {
		return errors.New("Config.MaxAttempts must be >= 1")
	}
	if c.ShutdownTimeout <= 0 {
		return errors.New("Config.ShutdownTimeout must be > 0")
	}
	return nil
}

// KindToQueue resolves a benchmark Kind to the queue it runs on. For the
// vendor-neutral contract every Kind runs on "default" unless an adapter
// explicitly routes it. Scenarios that want multi-queue behavior pass a
// custom Router through their Config.
type KindToQueue func(Kind) string

// DefaultRouter routes every Kind to "default".
func DefaultRouter(_ Kind) string { return "default" }

// Simulate emulates the worker's work for a job. It respects context
// cancellation and error injection. Adapters SHOULD call this from their
// registered worker-dispatch function rather than reimplementing it.
func Simulate(ctx context.Context, job Job) error {
	if job.Payload.FailWithError != "" {
		// The caller asked for a synthetic failure. Honor it regardless
		// of duration — tests expect the failure classification to apply.
		switch ErrorClass(job.Payload.FailWithError) {
		case ErrorRateLimit:
			return &SimulatedError{Class: ErrorRateLimit, Msg: "simulated rate limit"}
		case ErrorPermanent:
			return &SimulatedError{Class: ErrorPermanent, Msg: "simulated permanent error"}
		case ErrorTransient:
			return &SimulatedError{Class: ErrorTransient, Msg: "simulated transient error"}
		default:
			return &SimulatedError{Class: ErrorClass(job.Payload.FailWithError), Msg: "simulated error"}
		}
	}
	if job.Payload.WorkMillis <= 0 {
		return nil
	}
	select {
	case <-time.After(time.Duration(job.Payload.WorkMillis) * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SimulatedError is the error type returned by Simulate when a payload
// requests failure. Adapters can unwrap it to drive retry / cancellation
// decisions based on Class.
type SimulatedError struct {
	Class ErrorClass
	Msg   string
}

func (e *SimulatedError) Error() string { return e.Msg }

// IsSimulatedError returns the SimulatedError if err (or any wrapped
// error) is one, plus ok = true.
func IsSimulatedError(err error) (*SimulatedError, bool) {
	var se *SimulatedError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}
