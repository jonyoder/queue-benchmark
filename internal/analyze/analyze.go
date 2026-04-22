// Package analyze reads JSONL benchmark results and produces per-run
// summaries and cross-run comparisons. Pure functions over event streams
// so the same inputs always produce the same outputs.
package analyze

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunSummary captures aggregate statistics for one scenario run.
type RunSummary struct {
	RunID      string
	Library    string
	Scenario   string
	SeedInfo   map[string]any // params pulled from run_start event
	StartTime  time.Time
	EndTime    time.Time
	Duration   time.Duration

	Enqueued  int64
	Completed int64
	Failed    int64

	// Throughput = Completed / run wall-clock duration.
	ThroughputCPS float64 // completions per second

	// Pickup latency: job_enqueued.timestamp → job_started.timestamp, ms.
	PickupLatency LatencyStats
	// Work duration: job_started.timestamp → job_completed.timestamp, ms.
	WorkDuration LatencyStats
	// Total e2e: job_enqueued.timestamp → job_completed.timestamp, ms.
	E2ELatency LatencyStats

	// Error breakdown by class (for rate_limit / transient / permanent /
	// unclassified).
	Errors map[string]int64

	// Per-tenant counts & latency, for fairness analysis.
	PerTenant map[string]TenantStats

	// Resource usage snapshots over the run.
	MaxGoroutines int
	MaxRSSBytes   int64
}

// LatencyStats is a five-number summary in milliseconds.
type LatencyStats struct {
	Count int
	Min   float64
	P50   float64
	P95   float64
	P99   float64
	Max   float64
	Mean  float64
}

// TenantStats aggregates one tenant's contribution to a run.
type TenantStats struct {
	Enqueued       int64
	Completed      int64
	PickupLatency  LatencyStats
}

type rawEvent struct {
	RunID     string                 `json:"run_id"`
	Timestamp time.Time              `json:"timestamp"`
	Kind      string                 `json:"kind"`
	Fields    map[string]interface{} `json:"fields"`
}

// Analyze reads a single JSONL file and returns its summary.
func Analyze(path string) (RunSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return RunSummary{}, err
	}
	defer f.Close()

	return analyzeReader(f, filepath.Base(path))
}

func analyzeReader(r io.Reader, filename string) (RunSummary, error) {
	s := RunSummary{
		Errors:    make(map[string]int64),
		PerTenant: make(map[string]TenantStats),
	}

	enqueueTS := make(map[string]time.Time) // job_id → enqueued timestamp
	startTS := make(map[string]time.Time)   // job_id → started timestamp
	tenantByJob := make(map[string]string)  // job_id → tenant_id
	var pickupLatencies []float64
	var workDurations []float64
	var e2eLatencies []float64
	perTenantPickup := make(map[string][]float64)
	perTenantCounts := make(map[string]*TenantStats)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		var ev rawEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return s, fmt.Errorf("parse %s: %w", filename, err)
		}

		switch ev.Kind {
		case "run_start":
			s.StartTime = ev.Timestamp
			s.RunID = ev.RunID
			if lib, ok := ev.Fields["library"].(string); ok {
				s.Library = lib
			}
			if sc, ok := ev.Fields["scenario"].(string); ok {
				s.Scenario = sc
			}
			s.SeedInfo = ev.Fields

		case "run_end":
			s.EndTime = ev.Timestamp
			if c, ok := asInt64(ev.Fields["completed"]); ok {
				s.Completed = c
			}
			if c, ok := asInt64(ev.Fields["enqueued"]); ok {
				s.Enqueued = c
			}
			if c, ok := asInt64(ev.Fields["failed"]); ok {
				s.Failed = c
			}

		case "job_enqueued":
			jobID, _ := ev.Fields["job_id"].(string)
			tenantID, _ := ev.Fields["tenant_id"].(string)
			enqueueTS[jobID] = ev.Timestamp
			tenantByJob[jobID] = tenantID
			ts := perTenantCounts[tenantID]
			if ts == nil {
				ts = &TenantStats{}
				perTenantCounts[tenantID] = ts
			}
			ts.Enqueued++

		case "job_started":
			jobID, _ := ev.Fields["job_id"].(string)
			startTS[jobID] = ev.Timestamp
			if enq, ok := enqueueTS[jobID]; ok {
				latMs := float64(ev.Timestamp.Sub(enq).Microseconds()) / 1000.0
				pickupLatencies = append(pickupLatencies, latMs)
				if tenantID, ok := tenantByJob[jobID]; ok {
					perTenantPickup[tenantID] = append(perTenantPickup[tenantID], latMs)
				}
			}

		case "job_completed":
			jobID, _ := ev.Fields["job_id"].(string)
			tenantID, _ := ev.Fields["tenant_id"].(string)
			if st, ok := startTS[jobID]; ok {
				workDurations = append(workDurations, float64(ev.Timestamp.Sub(st).Microseconds())/1000.0)
			}
			if enq, ok := enqueueTS[jobID]; ok {
				e2eLatencies = append(e2eLatencies, float64(ev.Timestamp.Sub(enq).Microseconds())/1000.0)
			}
			if ts, ok := perTenantCounts[tenantID]; ok {
				ts.Completed++
			}

		case "job_failed":
			class, _ := ev.Fields["error_class"].(string)
			if class == "" {
				class = "unclassified"
			}
			s.Errors[class]++

		case "resource_sample":
			if g, ok := asInt64(ev.Fields["goroutines"]); ok {
				if int(g) > s.MaxGoroutines {
					s.MaxGoroutines = int(g)
				}
			}
			if rss, ok := asInt64(ev.Fields["rss_bytes"]); ok {
				if rss > s.MaxRSSBytes {
					s.MaxRSSBytes = rss
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return s, err
	}

	if s.StartTime.IsZero() || s.EndTime.IsZero() {
		return s, errors.New("run_start or run_end event missing")
	}
	s.Duration = s.EndTime.Sub(s.StartTime)
	if s.Duration > 0 && s.Completed > 0 {
		s.ThroughputCPS = float64(s.Completed) / s.Duration.Seconds()
	}

	s.PickupLatency = summarize(pickupLatencies)
	s.WorkDuration = summarize(workDurations)
	s.E2ELatency = summarize(e2eLatencies)

	for tid, ts := range perTenantCounts {
		ts.PickupLatency = summarize(perTenantPickup[tid])
		s.PerTenant[tid] = *ts
	}

	return s, nil
}

// AnalyzeDir reads every *.jsonl file in dir and returns their summaries
// sorted by (library, scenario, start_time).
func AnalyzeDir(dir string) ([]RunSummary, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var summaries []RunSummary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		s, err := Analyze(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		summaries = append(summaries, s)
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].Scenario != summaries[j].Scenario {
			return summaries[i].Scenario < summaries[j].Scenario
		}
		if summaries[i].Library != summaries[j].Library {
			return summaries[i].Library < summaries[j].Library
		}
		return summaries[i].StartTime.Before(summaries[j].StartTime)
	})
	return summaries, nil
}

func summarize(xs []float64) LatencyStats {
	if len(xs) == 0 {
		return LatencyStats{}
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	pct := func(p float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		idx := int(math.Ceil(p*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	sum := 0.0
	for _, v := range xs {
		sum += v
	}
	return LatencyStats{
		Count: len(xs),
		Min:   sorted[0],
		P50:   pct(0.50),
		P95:   pct(0.95),
		P99:   pct(0.99),
		Max:   sorted[len(sorted)-1],
		Mean:  sum / float64(len(xs)),
	}
}

func asInt64(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// RenderMarkdown produces a human-readable report from a set of run
// summaries, comparing libraries across each scenario they share.
func RenderMarkdown(runs []RunSummary) string {
	var b strings.Builder

	b.WriteString("# Queue Benchmark Results\n\n")
	if len(runs) == 0 {
		b.WriteString("_No runs found._\n")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("Runs analyzed: %d\n\n", len(runs)))

	// Group by scenario; within each, pair library runs.
	byScenario := make(map[string][]RunSummary)
	scenarioOrder := []string{}
	for _, r := range runs {
		if _, ok := byScenario[r.Scenario]; !ok {
			scenarioOrder = append(scenarioOrder, r.Scenario)
		}
		byScenario[r.Scenario] = append(byScenario[r.Scenario], r)
	}
	sort.Strings(scenarioOrder)

	for _, scn := range scenarioOrder {
		b.WriteString(fmt.Sprintf("## Scenario: `%s`\n\n", scn))
		runsInScn := byScenario[scn]
		renderScenarioTable(&b, runsInScn)
		renderPerTenantSection(&b, runsInScn)
		b.WriteString("\n")
	}

	b.WriteString("## Methodology\n\n")
	b.WriteString("Durations in milliseconds. Pickup latency is the time from when a job is ")
	b.WriteString("accepted into the queue to when a worker begins executing it. Work duration ")
	b.WriteString("is the synthetic `Simulate()` wall-clock inside the worker. E2E is the total ")
	b.WriteString("from enqueue to completion. See `docs/METHODOLOGY.md` for the full model.\n\n")

	return b.String()
}

func renderScenarioTable(b *strings.Builder, runs []RunSummary) {
	b.WriteString("| Library | Enqueued | Completed | Failed | Throughput (c/s) | Pickup p50 | Pickup p95 | Pickup p99 | Work p50 | E2E p95 | Max goroutines | Max RSS (MB) |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range runs {
		b.WriteString(fmt.Sprintf("| `%s` | %d | %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f | %.1f | %d | %.1f |\n",
			r.Library,
			r.Enqueued, r.Completed, r.Failed,
			r.ThroughputCPS,
			r.PickupLatency.P50, r.PickupLatency.P95, r.PickupLatency.P99,
			r.WorkDuration.P50,
			r.E2ELatency.P95,
			r.MaxGoroutines,
			float64(r.MaxRSSBytes)/1024.0/1024.0,
		))
	}
	b.WriteString("\n")

	// Error breakdown if any
	anyErrors := false
	for _, r := range runs {
		if len(r.Errors) > 0 {
			anyErrors = true
			break
		}
	}
	if anyErrors {
		b.WriteString("**Error breakdown by class:**\n\n")
		b.WriteString("| Library | rate_limit | transient | permanent | unclassified |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, r := range runs {
			b.WriteString(fmt.Sprintf("| `%s` | %d | %d | %d | %d |\n",
				r.Library,
				r.Errors["rate_limit"], r.Errors["transient"], r.Errors["permanent"], r.Errors["unclassified"],
			))
		}
		b.WriteString("\n")
	}
}

func renderPerTenantSection(b *strings.Builder, runs []RunSummary) {
	hasSkew := false
	for _, r := range runs {
		if len(r.PerTenant) < 2 {
			continue
		}
		// Look for meaningful skew — at least one tenant with 2x another's enqueues.
		maxEnq, minEnq := int64(0), int64(math.MaxInt64)
		for _, ts := range r.PerTenant {
			if ts.Enqueued > maxEnq {
				maxEnq = ts.Enqueued
			}
			if ts.Enqueued < minEnq {
				minEnq = ts.Enqueued
			}
		}
		if minEnq > 0 && float64(maxEnq)/float64(minEnq) > 2.0 {
			hasSkew = true
			break
		}
	}
	if !hasSkew {
		return
	}

	b.WriteString("**Per-tenant fairness:**\n\n")
	b.WriteString("| Library | Noisiest enqueued | Quietest enqueued | Noisy pickup p95 | Quiet pickup p95 | Fairness ratio (noisy / quiet p95) |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range runs {
		noisyID, quietID := pickExtremes(r.PerTenant)
		noisy := r.PerTenant[noisyID]
		quiet := r.PerTenant[quietID]
		ratio := 0.0
		if quiet.PickupLatency.P95 > 0 {
			ratio = noisy.PickupLatency.P95 / quiet.PickupLatency.P95
		}
		b.WriteString(fmt.Sprintf("| `%s` | %d | %d | %.1f | %.1f | %.2f |\n",
			r.Library,
			noisy.Enqueued, quiet.Enqueued,
			noisy.PickupLatency.P95, quiet.PickupLatency.P95,
			ratio,
		))
	}
	b.WriteString("\n")
	b.WriteString("_A fairness ratio near 1.0 means the noisy tenant doesn't starve the quiet ones. Ratios >2 mean meaningful starvation._\n\n")
}

func pickExtremes(per map[string]TenantStats) (noisy, quiet string) {
	var maxCnt, minCnt int64 = -1, math.MaxInt64
	for id, ts := range per {
		if ts.Enqueued > maxCnt {
			maxCnt = ts.Enqueued
			noisy = id
		}
		if ts.Enqueued < minCnt {
			minCnt = ts.Enqueued
			quiet = id
		}
	}
	return noisy, quiet
}
