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

// WarmupDefault is how much of the start of each run we skip before
// computing latency statistics. First-second effects (initial LISTEN/
// NOTIFY establishment, pool warmup) distort early numbers.
const WarmupDefault = 2 * time.Second

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

	// Determined from run_start after first pass. To stream-process we
	// defer latency additions until we know the warmup-end cutoff.
	var runStart time.Time
	warmupEnd := time.Time{}

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
			runStart = ev.Timestamp
			warmupEnd = runStart.Add(WarmupDefault)
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
			// Skip latency samples from the warmup window — they include
			// first-LISTEN-NOTIFY-establishment overhead that isn't
			// representative of steady-state library behavior.
			if !warmupEnd.IsZero() && ev.Timestamp.Before(warmupEnd) {
				break
			}
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
			if ts, ok := perTenantCounts[tenantID]; ok {
				ts.Completed++
			}
			if !warmupEnd.IsZero() && ev.Timestamp.Before(warmupEnd) {
				break
			}
			if st, ok := startTS[jobID]; ok {
				workDurations = append(workDurations, float64(ev.Timestamp.Sub(st).Microseconds())/1000.0)
			}
			if enq, ok := enqueueTS[jobID]; ok {
				e2eLatencies = append(e2eLatencies, float64(ev.Timestamp.Sub(enq).Microseconds())/1000.0)
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

// Group aggregates multiple runs with the same (library, scenario) pair.
// Medians + spreads across runs are the benchmark-interesting numbers;
// a single run can be noisy.
type Group struct {
	Library  string
	Scenario string
	Runs     []RunSummary

	// Aggregated percentiles across runs (median of each per-run value).
	MedThroughput   float64
	MedPickupP50    float64
	MedPickupP95    float64
	MedPickupP99    float64
	MedWorkP50      float64
	MedE2EP95       float64
	MinThroughput   float64
	MaxThroughput   float64
	MinPickupP95    float64
	MaxPickupP95    float64
	MedMaxGoroutines int
	MedMaxRSSMB     float64

	TotalEnqueued  int64
	TotalCompleted int64
	TotalFailed    int64
}

// GroupRuns buckets a slice of summaries by (library, scenario) and
// computes cross-run aggregates.
func GroupRuns(runs []RunSummary) []Group {
	byKey := make(map[string]*Group)
	for _, r := range runs {
		key := r.Library + "|" + r.Scenario
		g, ok := byKey[key]
		if !ok {
			g = &Group{Library: r.Library, Scenario: r.Scenario}
			byKey[key] = g
		}
		g.Runs = append(g.Runs, r)
	}
	out := make([]Group, 0, len(byKey))
	for _, g := range byKey {
		populateGroupStats(g)
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scenario != out[j].Scenario {
			return out[i].Scenario < out[j].Scenario
		}
		return out[i].Library < out[j].Library
	})
	return out
}

func populateGroupStats(g *Group) {
	if len(g.Runs) == 0 {
		return
	}
	thrs := make([]float64, len(g.Runs))
	p50s := make([]float64, len(g.Runs))
	p95s := make([]float64, len(g.Runs))
	p99s := make([]float64, len(g.Runs))
	w50s := make([]float64, len(g.Runs))
	e95s := make([]float64, len(g.Runs))
	goros := make([]int, len(g.Runs))
	rssMB := make([]float64, len(g.Runs))
	for i, r := range g.Runs {
		thrs[i] = r.ThroughputCPS
		p50s[i] = r.PickupLatency.P50
		p95s[i] = r.PickupLatency.P95
		p99s[i] = r.PickupLatency.P99
		w50s[i] = r.WorkDuration.P50
		e95s[i] = r.E2ELatency.P95
		goros[i] = r.MaxGoroutines
		rssMB[i] = float64(r.MaxRSSBytes) / 1024.0 / 1024.0
		g.TotalEnqueued += r.Enqueued
		g.TotalCompleted += r.Completed
		g.TotalFailed += r.Failed
	}
	g.MedThroughput = medianF(thrs)
	g.MedPickupP50 = medianF(p50s)
	g.MedPickupP95 = medianF(p95s)
	g.MedPickupP99 = medianF(p99s)
	g.MedWorkP50 = medianF(w50s)
	g.MedE2EP95 = medianF(e95s)
	g.MinThroughput = minF(thrs)
	g.MaxThroughput = maxF(thrs)
	g.MinPickupP95 = minF(p95s)
	g.MaxPickupP95 = maxF(p95s)
	g.MedMaxGoroutines = int(medianF(float64Ints(goros)))
	g.MedMaxRSSMB = medianF(rssMB)
}

func medianF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

func minF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func maxF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

func float64Ints(xs []int) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = float64(x)
	}
	return out
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
	groups := GroupRuns(runs)
	b.WriteString(fmt.Sprintf("Runs analyzed: %d (across %d scenario × library pairs)\n\n", len(runs), len(groups)))

	// Group by scenario; within each, pair library groups.
	scenarios := map[string][]Group{}
	order := []string{}
	for _, g := range groups {
		if _, ok := scenarios[g.Scenario]; !ok {
			order = append(order, g.Scenario)
		}
		scenarios[g.Scenario] = append(scenarios[g.Scenario], g)
	}
	sort.Strings(order)

	for _, scn := range order {
		b.WriteString(fmt.Sprintf("## Scenario: `%s`\n\n", scn))
		scnGroups := scenarios[scn]
		renderScenarioGroupTable(&b, scnGroups)
		renderErrorBreakdown(&b, scnGroups)
		renderScenarioRunsTable(&b, scnGroups)
		renderPerTenantSection(&b, flattenRuns(scnGroups))
		b.WriteString("\n")
	}

	b.WriteString("## Methodology\n\n")
	b.WriteString("Durations in milliseconds. **Values are medians across runs** where multiple runs were collected; single-run cells show the one measurement. Min/max columns show the spread across runs — a tight spread means the measurement is stable; a wide spread means the measurement itself is noisy and should be interpreted carefully.\n\n")
	b.WriteString("Pickup latency is the time from when a job is accepted into the queue (after `Enqueue()` returns) to when a worker begins executing it. Work duration is the synthetic `Simulate()` wall-clock inside the worker. E2E is the total from enqueue to completion. The first ")
	b.WriteString(fmt.Sprintf("%s", WarmupDefault))
	b.WriteString(" of each run is excluded from latency statistics as warmup.\n\n")
	b.WriteString("See `docs/METHODOLOGY.md` for the full model.\n\n")

	return b.String()
}

func flattenRuns(groups []Group) []RunSummary {
	var out []RunSummary
	for _, g := range groups {
		out = append(out, g.Runs...)
	}
	return out
}

func renderScenarioGroupTable(b *strings.Builder, groups []Group) {
	b.WriteString("| Library | Runs | Throughput (c/s) med (min–max) | Pickup p50 (med) | Pickup p95 (med, min–max) | Pickup p99 (med) | Work p50 | E2E p95 | Goroutines (med max) | RSS MB (med max) |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, g := range groups {
		b.WriteString(fmt.Sprintf(
			"| `%s` | %d | **%.1f** (%.1f–%.1f) | %.2f | **%.1f** (%.1f–%.1f) | %.1f | %.1f | %.1f | %d | %.1f |\n",
			g.Library, len(g.Runs),
			g.MedThroughput, g.MinThroughput, g.MaxThroughput,
			g.MedPickupP50,
			g.MedPickupP95, g.MinPickupP95, g.MaxPickupP95,
			g.MedPickupP99,
			g.MedWorkP50,
			g.MedE2EP95,
			g.MedMaxGoroutines,
			g.MedMaxRSSMB,
		))
	}
	b.WriteString("\n")
}

func renderErrorBreakdown(b *strings.Builder, groups []Group) {
	// Sum errors across each group's runs.
	totals := make([]map[string]int64, len(groups))
	any := false
	for i, g := range groups {
		totals[i] = make(map[string]int64)
		for _, r := range g.Runs {
			for cls, cnt := range r.Errors {
				totals[i][cls] += cnt
				if cnt > 0 {
					any = true
				}
			}
		}
	}
	if !any {
		return
	}
	b.WriteString("**Error breakdown (total across runs):**\n\n")
	b.WriteString("| Library | rate_limit | transient | permanent | unclassified |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for i, g := range groups {
		b.WriteString(fmt.Sprintf("| `%s` | %d | %d | %d | %d |\n",
			g.Library,
			totals[i]["rate_limit"], totals[i]["transient"], totals[i]["permanent"], totals[i]["unclassified"],
		))
	}
	b.WriteString("\n")
}

func renderScenarioRunsTable(b *strings.Builder, groups []Group) {
	// Only show per-run breakdown when there are multiple runs per group.
	hasMulti := false
	for _, g := range groups {
		if len(g.Runs) > 1 {
			hasMulti = true
			break
		}
	}
	if !hasMulti {
		return
	}
	b.WriteString("<details><summary>Per-run breakdown</summary>\n\n")
	b.WriteString("| Library | Run | Enqueued | Completed | Failed | Throughput | Pickup p50 | Pickup p95 | Pickup p99 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, g := range groups {
		for i, r := range g.Runs {
			b.WriteString(fmt.Sprintf(
				"| `%s` | %d | %d | %d | %d | %.1f | %.1f | %.1f | %.1f |\n",
				g.Library, i+1,
				r.Enqueued, r.Completed, r.Failed,
				r.ThroughputCPS,
				r.PickupLatency.P50, r.PickupLatency.P95, r.PickupLatency.P99,
			))
		}
	}
	b.WriteString("\n</details>\n\n")
}

// Error breakdown is surfaced by renderScenarioGroupTable when error
// counts are present in the group's runs.

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
