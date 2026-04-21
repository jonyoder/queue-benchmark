// Command bench is the benchmark CLI entry point. Subcommands:
//
//	bench run    — run a single scenario against a single library.
//	bench report — analyze the collected JSONL results.
//
// This file is deliberately minimal in Phase 1. Workload-generation,
// adapter wiring, and analysis logic land in subsequent phases.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "report":
		cmdReport(os.Args[2:])
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
  bench run    --lib=<name> --scenario=<name> --postgres-url=<url>
  bench report --results-dir=<path>

`)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	lib := fs.String("lib", "", "library adapter (river|platlib)")
	scenario := fs.String("scenario", "", "scenario name")
	pgURL := fs.String("postgres-url", "", "Postgres connection URL")
	resultsDir := fs.String("results-dir", "./results", "where to write JSONL results")
	_ = fs.Parse(args)

	if *lib == "" || *scenario == "" || *pgURL == "" {
		fmt.Fprintln(os.Stderr, "run: --lib, --scenario, and --postgres-url are required")
		os.Exit(2)
	}
	_ = resultsDir

	// Phase 1 stub: wire-up lives in Phase 2 (adapters) + Phase 3 (scenarios).
	fmt.Fprintf(os.Stderr, "run: not yet implemented (lib=%s, scenario=%s)\n", *lib, *scenario)
	os.Exit(1)
}

func cmdReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	resultsDir := fs.String("results-dir", "./results", "directory of JSONL results to analyze")
	_ = fs.Parse(args)
	_ = resultsDir

	// Phase 1 stub: analysis lives in Phase 4.
	fmt.Fprintln(os.Stderr, "report: not yet implemented")
	os.Exit(1)
}
