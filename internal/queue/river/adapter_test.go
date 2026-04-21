package river_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
	"github.com/jonyoder/queue-benchmark/internal/queue/river"
	"github.com/jonyoder/queue-benchmark/internal/queue/testhelp"
)

// newHarness spins up a fresh River harness against the test Postgres.
// Schema is reset between tests so each test starts clean.
func newHarness(t *testing.T) *river.Harness {
	t.Helper()
	url := testhelp.PostgresURL(t)
	testhelp.TruncateAll(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h, err := river.New(ctx, qpkg.Config{
		PostgresURL: url,
		Queues: map[string]qpkg.QueueConfig{
			"default": {MaxWorkers: 2},
		},
		MaxAttempts:     3,
		ShutdownTimeout: 5 * time.Second,
	}, river.Options{})
	if err != nil {
		t.Fatalf("river.New: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = h.Stop(stopCtx)
	})
	return h
}

func TestEnqueueAndProcess(t *testing.T) {
	h := newHarness(t)

	var processed atomic.Int32
	done := make(chan qpkg.Job, 1)

	if err := h.Register("document_process", func(ctx context.Context, job qpkg.Job) error {
		processed.Add(1)
		// Non-blocking send; main test goroutine drains.
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
	jobID, err := h.Enqueue(ctx, tenantID, "document_process", qpkg.JobPayload{
		WorkMillis: 20,
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if jobID == "" {
		t.Error("Enqueue returned empty JobID")
	}

	select {
	case got := <-done:
		if got.Kind != "document_process" {
			t.Errorf("job.Kind = %q, want document_process", got.Kind)
		}
		if got.TenantID != tenantID {
			t.Errorf("job.TenantID = %v, want %v", got.TenantID, tenantID)
		}
		if got.Payload.WorkMillis != 20 {
			t.Errorf("job.Payload.WorkMillis = %d, want 20", got.Payload.WorkMillis)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("worker did not process the job within 10s (processed count: %d)", processed.Load())
	}
}

func TestRegisterAfterStart(t *testing.T) {
	h := newHarness(t)
	_ = h.Register("document_process", func(_ context.Context, _ qpkg.Job) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err := h.Register("entity_update", func(_ context.Context, _ qpkg.Job) error { return nil })
	if err == nil {
		t.Fatal("Register after Start should fail")
	}
}

func TestBulkEnqueue(t *testing.T) {
	h := newHarness(t)

	var processed atomic.Int32
	const n = 5
	processedN := make(chan struct{}, n)

	if err := h.Register("entity_update", func(ctx context.Context, job qpkg.Job) error {
		processed.Add(1)
		select {
		case processedN <- struct{}{}:
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
	reqs := make([]qpkg.EnqueueRequest, n)
	for i := range reqs {
		reqs[i] = qpkg.EnqueueRequest{
			TenantID: tenantID,
			Kind:     "entity_update",
			Payload:  qpkg.JobPayload{WorkMillis: 10},
		}
	}
	ids, err := h.EnqueueMany(ctx, reqs)
	if err != nil {
		t.Fatalf("EnqueueMany: %v", err)
	}
	if len(ids) != n {
		t.Fatalf("EnqueueMany returned %d ids, want %d", len(ids), n)
	}

	// Wait for all n to land.
	seen := 0
	timeout := time.After(15 * time.Second)
	for seen < n {
		select {
		case <-processedN:
			seen++
		case <-timeout:
			t.Fatalf("only %d of %d jobs processed within 15s", seen, n)
		}
	}
}

func TestPermanentErrorDoesNotRetry(t *testing.T) {
	h := newHarness(t)

	var attempts atomic.Int32
	if err := h.Register("document_process", func(ctx context.Context, job qpkg.Job) error {
		attempts.Add(1)
		return qpkg.Simulate(ctx, job)
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := h.Enqueue(ctx, uuid.New(), "document_process", qpkg.JobPayload{
		FailWithError: "permanent",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// A Permanent simulated error maps to river.JobCancel in the adapter;
	// the job should run exactly once and not retry.
	time.Sleep(3 * time.Second)
	if got := attempts.Load(); got != 1 {
		t.Errorf("permanent-error job ran %d times, want exactly 1 (JobCancel expected)", got)
	}
}
