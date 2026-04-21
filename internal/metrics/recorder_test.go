package metrics

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRecorderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir, "test-run-1")
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	if err := r.Record(KindJobEnqueued, map[string]interface{}{
		"job_id":    "j-1",
		"tenant_id": "t-1",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := r.Record(KindJobCompleted, map[string]interface{}{
		"job_id":     "j-1",
		"duration_ms": 42,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read back and validate both records landed with the envelope we expect.
	path := filepath.Join(dir, "test-run-1.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open results file: %v", err)
	}
	defer f.Close()

	var events []Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("decode event: %v (line=%q)", err, scanner.Text())
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if got, want := events[0].RunID, "test-run-1"; got != want {
		t.Errorf("event[0].RunID = %q, want %q", got, want)
	}
	if got, want := events[0].Kind, KindJobEnqueued; got != want {
		t.Errorf("event[0].Kind = %q, want %q", got, want)
	}
	if events[0].Timestamp.IsZero() {
		t.Error("event[0].Timestamp is zero")
	}
	if events[0].Fields["job_id"] != "j-1" {
		t.Errorf("event[0].Fields.job_id = %v, want j-1", events[0].Fields["job_id"])
	}
	if events[1].Kind != KindJobCompleted {
		t.Errorf("event[1].Kind = %q, want %q", events[1].Kind, KindJobCompleted)
	}
}

func TestRecorderConcurrent(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir, "concurrent")
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer r.Close()

	const goroutines = 16
	const recordsPer = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < recordsPer; j++ {
				if err := r.Record(KindJobEnqueued, map[string]interface{}{
					"goroutine": id, "iter": j,
				}); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify every write survived and is a valid JSON line. The recorder's
	// contract is that lines don't interleave; a corrupt line would fail
	// to parse.
	path := filepath.Join(dir, "concurrent.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("decode line %d: %v (%q)", count, err, scanner.Text())
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != goroutines*recordsPer {
		t.Errorf("got %d lines, want %d", count, goroutines*recordsPer)
	}
}
