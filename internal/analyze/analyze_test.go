package analyze

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// syntheticRun builds a JSONL event stream describing a run with a known
// answer so we can unit-test the analyzer's arithmetic.
func syntheticRun(runID, library, scenario string, pickupLatenciesMs []float64, start time.Time) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"run_id":%q,"timestamp":%q,"kind":"run_start","fields":{"library":%q,"scenario":%q,"duration_sec":10}}`+"\n",
		runID, start.UTC().Format(time.RFC3339Nano), library, scenario)
	// Put the jobs after warmup to avoid the 2-second filter discarding
	// half our synthetic samples.
	baseline := start.Add(5 * time.Second)
	for i, lat := range pickupLatenciesMs {
		enq := baseline.Add(time.Duration(i) * 100 * time.Millisecond)
		started := enq.Add(time.Duration(lat * float64(time.Millisecond)))
		completed := started.Add(20 * time.Millisecond)
		jid := fmt.Sprintf("j-%d", i)
		tid := "t-A"

		fmt.Fprintf(&b, `{"run_id":%q,"timestamp":%q,"kind":"job_enqueued","fields":{"job_id":%q,"tenant_id":%q,"kind":"entity_update"}}`+"\n",
			runID, enq.UTC().Format(time.RFC3339Nano), jid, tid)
		fmt.Fprintf(&b, `{"run_id":%q,"timestamp":%q,"kind":"job_started","fields":{"job_id":%q,"tenant_id":%q,"kind":"entity_update"}}`+"\n",
			runID, started.UTC().Format(time.RFC3339Nano), jid, tid)
		fmt.Fprintf(&b, `{"run_id":%q,"timestamp":%q,"kind":"job_completed","fields":{"job_id":%q,"tenant_id":%q,"kind":"entity_update","duration_ms":20}}`+"\n",
			runID, completed.UTC().Format(time.RFC3339Nano), jid, tid)
	}
	end := start.Add(10 * time.Second)
	fmt.Fprintf(&b, `{"run_id":%q,"timestamp":%q,"kind":"run_end","fields":{"enqueued":%d,"completed":%d,"failed":0}}`+"\n",
		runID, end.UTC().Format(time.RFC3339Nano), len(pickupLatenciesMs), len(pickupLatenciesMs))
	return b.Bytes()
}

func TestAnalyzerLatencyMath(t *testing.T) {
	// 100 samples with uniform latency 50ms → every percentile should be 50.
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = 50.0
	}
	data := syntheticRun("test-1", "river", "unit", samples, time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC))

	s, err := analyzeReader(bytes.NewReader(data), "test-1.jsonl")
	if err != nil {
		t.Fatalf("analyzeReader: %v", err)
	}
	if s.PickupLatency.Count != 100 {
		t.Fatalf("expected 100 pickup samples, got %d", s.PickupLatency.Count)
	}
	if math.Abs(s.PickupLatency.P50-50.0) > 0.5 {
		t.Errorf("p50 = %f, want ~50", s.PickupLatency.P50)
	}
	if math.Abs(s.PickupLatency.P95-50.0) > 0.5 {
		t.Errorf("p95 = %f, want ~50", s.PickupLatency.P95)
	}
	if math.Abs(s.PickupLatency.P99-50.0) > 0.5 {
		t.Errorf("p99 = %f, want ~50", s.PickupLatency.P99)
	}
}

func TestAnalyzerPercentileOrdering(t *testing.T) {
	// Linear ramp: 1ms, 2ms, ..., 100ms. p50 should be around 50,
	// p95 around 95, p99 around 99, max 100.
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = float64(i + 1)
	}
	data := syntheticRun("test-2", "river", "unit", samples, time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC))

	s, err := analyzeReader(bytes.NewReader(data), "test-2.jsonl")
	if err != nil {
		t.Fatalf("analyzeReader: %v", err)
	}
	if s.PickupLatency.Count != 100 {
		t.Fatalf("got %d samples, want 100", s.PickupLatency.Count)
	}
	// P50 on 100 samples is sample 50 in 1-indexed ceiling semantics.
	if int(s.PickupLatency.P50) != 50 {
		t.Errorf("p50 = %v, want 50", s.PickupLatency.P50)
	}
	if int(s.PickupLatency.P95) != 95 {
		t.Errorf("p95 = %v, want 95", s.PickupLatency.P95)
	}
	if int(s.PickupLatency.P99) != 99 {
		t.Errorf("p99 = %v, want 99", s.PickupLatency.P99)
	}
	if int(s.PickupLatency.Max) != 100 {
		t.Errorf("max = %v, want 100", s.PickupLatency.Max)
	}
}

func TestAnalyzerWarmupFilter(t *testing.T) {
	// Samples in the first 2s of the run should be discarded. Use a
	// synthetic run where all jobs enqueue at t+0 (inside warmup) —
	// analyzer should report zero latency samples.
	start := time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"run_id":"w","timestamp":%q,"kind":"run_start","fields":{"library":"river","scenario":"unit","duration_sec":10}}`+"\n",
		start.UTC().Format(time.RFC3339Nano))
	for i := 0; i < 10; i++ {
		enq := start.Add(time.Duration(100*i) * time.Millisecond) // all under 2s
		started := enq.Add(5 * time.Millisecond)
		completed := started.Add(1 * time.Millisecond)
		jid := fmt.Sprintf("w-%d", i)
		fmt.Fprintf(&b, `{"run_id":"w","timestamp":%q,"kind":"job_enqueued","fields":{"job_id":%q,"tenant_id":"t","kind":"entity_update"}}`+"\n",
			enq.UTC().Format(time.RFC3339Nano), jid)
		fmt.Fprintf(&b, `{"run_id":"w","timestamp":%q,"kind":"job_started","fields":{"job_id":%q,"tenant_id":"t","kind":"entity_update"}}`+"\n",
			started.UTC().Format(time.RFC3339Nano), jid)
		fmt.Fprintf(&b, `{"run_id":"w","timestamp":%q,"kind":"job_completed","fields":{"job_id":%q,"tenant_id":"t","kind":"entity_update"}}`+"\n",
			completed.UTC().Format(time.RFC3339Nano), jid)
	}
	end := start.Add(10 * time.Second)
	fmt.Fprintf(&b, `{"run_id":"w","timestamp":%q,"kind":"run_end","fields":{"enqueued":10,"completed":10,"failed":0}}`+"\n",
		end.UTC().Format(time.RFC3339Nano))

	s, err := analyzeReader(&b, "w.jsonl")
	if err != nil {
		t.Fatalf("analyzeReader: %v", err)
	}
	if s.PickupLatency.Count != 0 {
		t.Errorf("expected 0 pickup samples (all inside warmup), got %d", s.PickupLatency.Count)
	}
	if s.Completed != 10 {
		t.Errorf("run_end completed = %d, want 10 (warmup filter shouldn't affect counts)", s.Completed)
	}
}

func TestGroupRunsAggregation(t *testing.T) {
	start := time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC)

	// Three runs of the same (library, scenario) with throughputs 30, 35, 40.
	// Median should be 35.
	var runs []RunSummary
	for i, thr := range []float64{30, 35, 40} {
		runID := fmt.Sprintf("r-%d", i)
		// Build with constant latency so the per-run stats are predictable.
		samples := make([]float64, 100)
		for j := range samples {
			samples[j] = 10.0
		}
		data := syntheticRun(runID, "river", "shared", samples, start.Add(time.Duration(i)*time.Minute))
		s, err := analyzeReader(bytes.NewReader(data), runID+".jsonl")
		if err != nil {
			t.Fatalf("analyzeReader[%d]: %v", i, err)
		}
		// Override throughput so we can test median aggregation deterministically.
		s.ThroughputCPS = thr
		runs = append(runs, s)
	}

	groups := GroupRuns(runs)
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Library != "river" || g.Scenario != "shared" {
		t.Errorf("unexpected group key: %s/%s", g.Library, g.Scenario)
	}
	if len(g.Runs) != 3 {
		t.Errorf("want 3 runs in group, got %d", len(g.Runs))
	}
	if g.MedThroughput != 35 {
		t.Errorf("median throughput = %v, want 35", g.MedThroughput)
	}
	if g.MinThroughput != 30 || g.MaxThroughput != 40 {
		t.Errorf("throughput range [%v, %v], want [30, 40]", g.MinThroughput, g.MaxThroughput)
	}
}

func TestRenderMarkdownMinimal(t *testing.T) {
	start := time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC)
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = 10.0
	}
	data := syntheticRun("m-1", "river", "unit", samples, start)
	s, err := analyzeReader(bytes.NewReader(data), "m-1.jsonl")
	if err != nil {
		t.Fatalf("analyzeReader: %v", err)
	}
	md := RenderMarkdown([]RunSummary{s})
	for _, needle := range []string{"# Queue Benchmark Results", "## Scenario: `unit`", "`river`"} {
		if !strings.Contains(md, needle) {
			t.Errorf("rendered report missing %q", needle)
		}
	}
}
