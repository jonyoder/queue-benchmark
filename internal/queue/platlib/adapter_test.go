package platlib_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
	"github.com/jonyoder/queue-benchmark/internal/queue/platlib"
	"github.com/jonyoder/queue-benchmark/internal/queue/testhelp"
)

func newHarness(t *testing.T, opts platlib.Options) *platlib.Harness {
	t.Helper()
	url := testhelp.PostgresURL(t)
	testhelp.TruncateAll(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h, err := platlib.New(ctx, qpkg.Config{
		PostgresURL: url,
		Queues: map[string]qpkg.QueueConfig{
			"default": {MaxWorkers: 2},
		},
		MaxAttempts:     3,
		ShutdownTimeout: 5 * time.Second,
	}, opts)
	if err != nil {
		t.Fatalf("platlib.New: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = h.Stop(stopCtx)
	})
	return h
}

func TestEnqueueAndProcess(t *testing.T) {
	h := newHarness(t, platlib.Options{})

	done := make(chan qpkg.Job, 1)
	var processed atomic.Int32

	if err := h.Register("document_process", func(ctx context.Context, job qpkg.Job) error {
		processed.Add(1)
		select {
		case done <- job:
		default:
		}
		return qpkg.Simulate(ctx, job)
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	tenantID := uuid.New()
	if _, err := h.Enqueue(ctx, tenantID, "document_process", qpkg.JobPayload{
		WorkMillis: 10,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	select {
	case got := <-done:
		if got.Kind != "document_process" {
			t.Errorf("job.Kind = %q, want document_process", got.Kind)
		}
		if got.TenantID != tenantID {
			t.Errorf("job.TenantID = %v, want %v", got.TenantID, tenantID)
		}
		if got.Payload.WorkMillis != 10 {
			t.Errorf("job.Payload.WorkMillis = %d, want 10", got.Payload.WorkMillis)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("worker did not process within 15s (processed: %d)", processed.Load())
	}
}

func TestTransientErrorRetries(t *testing.T) {
	h := newHarness(t, platlib.Options{})

	var attempts atomic.Int32
	done := make(chan struct{}, 1)

	// Worker fails twice, then succeeds on the third attempt.
	if err := h.Register("entity_update", func(ctx context.Context, job qpkg.Job) error {
		n := attempts.Add(1)
		if n < 3 {
			return &qpkg.SimulatedError{Class: qpkg.ErrorTransient, Msg: "still failing"}
		}
		select {
		case done <- struct{}{}:
		default:
		}
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := h.Enqueue(ctx, uuid.New(), "entity_update", qpkg.JobPayload{WorkMillis: 1}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	select {
	case <-done:
		// Expect exactly 3 attempts: fail, fail, success.
		if got := attempts.Load(); got != 3 {
			t.Errorf("attempts = %d, want 3 (fail, fail, success)", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("job never succeeded (attempts: %d)", attempts.Load())
	}
}

func TestPermanentErrorDoesNotRetry(t *testing.T) {
	h := newHarness(t, platlib.Options{})

	var attempts atomic.Int32
	if err := h.Register("entity_update", func(ctx context.Context, job qpkg.Job) error {
		attempts.Add(1)
		return &qpkg.SimulatedError{Class: qpkg.ErrorPermanent, Msg: "not recoverable"}
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := h.Enqueue(ctx, uuid.New(), "entity_update", qpkg.JobPayload{
		WorkMillis:    1,
		FailWithError: "permanent",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Permanent errors should cause exactly one attempt.
	time.Sleep(3 * time.Second)
	if got := attempts.Load(); got != 1 {
		t.Errorf("permanent-error job ran %d times, want exactly 1", got)
	}
}

func TestAddressedPushDedupe(t *testing.T) {
	h := newHarness(t, platlib.Options{UseAddressedPush: true})

	var processed atomic.Int32
	gate := make(chan struct{})

	if err := h.Register("document_process", func(ctx context.Context, job qpkg.Job) error {
		processed.Add(1)
		// Hold the worker busy so a second AddressedPush collides.
		<-gate
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Release the worker when the test ends.
	defer close(gate)

	// The Enqueue helper uses a per-call unique address (bench:<uuid>),
	// so two Enqueue calls won't collide on address. We exercise the
	// AddressedPush path here — dedup semantics are covered by the
	// store unit tests separately. For now, just validate the path works.
	tenantID := uuid.New()
	if _, err := h.Enqueue(ctx, tenantID, "document_process", qpkg.JobPayload{WorkMillis: 1}); err != nil {
		t.Fatalf("AddressedPush Enqueue: %v", err)
	}
	// Give the worker a moment to pick it up.
	deadline := time.Now().Add(10 * time.Second)
	for processed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processed.Load() == 0 {
		t.Fatal("AddressedPush work did not reach the worker within 10s")
	}
}
