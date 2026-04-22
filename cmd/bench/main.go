// Command bench is the benchmark CLI entry point.
//
//	bench run    — run a single scenario against a single library.
//	bench report — analyze collected JSONL results (Phase 4).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonyoder/queue-benchmark/internal/analyze"
	"github.com/jonyoder/queue-benchmark/internal/metrics"
	qpkg "github.com/jonyoder/queue-benchmark/internal/queue"
	"github.com/jonyoder/queue-benchmark/internal/queue/platlib"
	"github.com/jonyoder/queue-benchmark/internal/queue/river"
	"github.com/jonyoder/queue-benchmark/internal/runner"
	"github.com/jonyoder/queue-benchmark/internal/workload"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		if err := cmdRun(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "report":
		if err := cmdReport(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `bench — queue-benchmark CLI

Usage:
  bench run    --lib=<river|platlib> --scenario=<name> --postgres-url=<url> [flags]
  bench report --results-dir=<path>                            (Phase 4)

Scenarios: steady, burst, noisy_neighbor, rate_limit_pressure, notify_latency

Flags for run:
  --lib              river | platlib
  --scenario         built-in scenario name
  --postgres-url     Postgres connection URL
  --results-dir      output dir for JSONL results (default ./results)
  --workers          harness worker count (default 10)
  --addressed-push   platform-lib only: use AddressedPush instead of Push

`)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	lib := fs.String("lib", "", "library adapter (river|platlib)")
	scenario := fs.String("scenario", "", "scenario name")
	pgURL := fs.String("postgres-url", "", "Postgres connection URL")
	resultsDir := fs.String("results-dir", "./results", "where to write JSONL results")
	workers := fs.Int("workers", 10, "harness worker count")
	addressedPush := fs.Bool("addressed-push", false, "platform-lib only: use AddressedPush")
	duration := fs.Duration("duration", 0, "override scenario duration (e.g., 30s)")
	rate := fs.Int("rate", 0, "override scenario enqueue rate (jobs/sec)")
	tenants := fs.Int("tenants", 0, "override scenario tenant count")
	backoff := fs.Bool("backoff", false, "platform-lib only: enable River-like exponential backoff on retry")
	_ = fs.Parse(args)

	if *lib == "" || *scenario == "" || *pgURL == "" {
		fs.Usage()
		return fmt.Errorf("run: --lib, --scenario, --postgres-url are required")
	}

	spec, err := workload.SpecFor(workload.Scenario(*scenario))
	if err != nil {
		return fmt.Errorf("scenario: %w", err)
	}
	if *duration > 0 {
		spec.Duration = *duration
	}
	if *rate > 0 {
		spec.EnqueueRate = *rate
	}
	if *tenants > 0 {
		spec.Tenants = *tenants
	}

	// Ctrl-C aware context for clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := qpkg.Config{
		PostgresURL: *pgURL,
		Queues: map[string]qpkg.QueueConfig{
			"default": {MaxWorkers: *workers},
		},
		MaxAttempts:     3,
		ShutdownTimeout: 30 * time.Second,
	}

	var harness qpkg.Harness
	switch *lib {
	case "river":
		h, err := river.New(ctx, cfg, river.Options{})
		if err != nil {
			return err
		}
		harness = h
	case "platlib":
		pOpts := platlib.Options{UseAddressedPush: *addressedPush}
		if *backoff {
			pOpts.BackoffFn = platlib.ExponentialBackoffRiverLike
		}
		h, err := platlib.New(ctx, cfg, pOpts)
		if err != nil {
			return err
		}
		harness = h
	default:
		return fmt.Errorf("unknown library %q (expected river or platlib)", *lib)
	}

	runID := fmt.Sprintf("%s-%s-%d", *lib, *scenario, time.Now().Unix())
	rec, err := metrics.NewRecorder(*resultsDir, runID)
	if err != nil {
		return err
	}
	defer rec.Close()

	fmt.Fprintf(os.Stdout, "run %s: library=%s scenario=%s duration=%s enqueue_rate=%d\n",
		runID, *lib, *scenario, spec.Duration, spec.EnqueueRate)

	if err := runner.Run(ctx, runner.Config{
		Library:  *lib,
		Harness:  harness,
		Spec:     spec,
		Recorder: rec,
	}); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "run %s done: results written to %s/%s.jsonl\n", runID, *resultsDir, runID)
	return nil
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	resultsDir := fs.String("results-dir", "./results", "directory of JSONL results")
	out := fs.String("out", "", "optional output path for Markdown report (stdout if empty)")
	_ = fs.Parse(args)

	summaries, err := analyze.AnalyzeDir(*resultsDir)
	if err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	md := analyze.RenderMarkdown(summaries)
	if *out == "" {
		fmt.Fprint(os.Stdout, md)
		return nil
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stdout, "report: wrote %s\n", *out)
	return nil
}
