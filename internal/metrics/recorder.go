// Package metrics records benchmark events as newline-delimited JSON for
// deterministic post-run analysis. Every event is a structured record
// with a common envelope (run_id, timestamp, kind) plus kind-specific
// fields.
//
// The writer is append-only and safe for concurrent callers. Events are
// NOT lost on crash provided the process exits cleanly enough to flush
// the underlying file.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event kinds recognized by the analyzer. Unknown kinds are preserved
// verbatim in the JSONL stream; the analyzer tolerates them.
const (
	KindRunStart        = "run_start"
	KindRunEnd          = "run_end"
	KindJobEnqueued     = "job_enqueued"
	KindJobStarted      = "job_started"
	KindJobCompleted    = "job_completed"
	KindJobFailed       = "job_failed"
	KindJobRetried      = "job_retried"
	KindJobDiscarded    = "job_discarded"
	KindQueueSnapshot   = "queue_snapshot"
	KindResourceSample  = "resource_sample"
)

// Event is the envelope every record shares. The Fields map holds kind-
// specific data.
type Event struct {
	RunID     string                 `json:"run_id"`
	Timestamp time.Time              `json:"timestamp"`
	Kind      string                 `json:"kind"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

// Recorder writes Events as JSONL to an underlying file. One Recorder
// per run.
type Recorder struct {
	runID string
	mu    sync.Mutex
	enc   *json.Encoder
	file  io.WriteCloser
}

// NewRecorder creates a JSONL recorder writing to results/<runID>.jsonl
// relative to the given directory.
func NewRecorder(dir, runID string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir results dir: %w", err)
	}
	path := filepath.Join(dir, runID+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open results file: %w", err)
	}
	return &Recorder{
		runID: runID,
		enc:   json.NewEncoder(f),
		file:  f,
	}, nil
}

// Record writes a single event. Safe for concurrent use.
func (r *Recorder) Record(kind string, fields map[string]interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enc.Encode(Event{
		RunID:     r.runID,
		Timestamp: time.Now().UTC(),
		Kind:      kind,
		Fields:    fields,
	})
}

// Close flushes and closes the underlying file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}
