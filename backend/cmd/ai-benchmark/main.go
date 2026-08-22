// Command ai-benchmark runs the labelled golden set against a llama.cpp
// OpenAI-compatible endpoint and emits a machine-readable report.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"lidradar/backend/internal/ai/benchmark"
	"lidradar/backend/internal/ai/infrastructure"
)

func main() {
	datasetPath := flag.String("dataset", "models/datasets/golden_v1.jsonl", "versioned JSONL dataset")
	digestPath := flag.String("checksum", "models/datasets/golden_v1.sha256", "reviewed dataset checksum")
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080/v1/chat/completions", "llama.cpp endpoint")
	timeout := flag.Duration("timeout", 30*time.Minute, "whole benchmark timeout")
	precision := flag.Float64("minimum-precision", 0, "accepted precision threshold (required)")
	recall := flag.Float64("minimum-recall", 0, "accepted recall threshold (required)")
	f1 := flag.Float64("minimum-f1", 0, "accepted F1 threshold (required)")
	exact := flag.Float64("minimum-exact-rate", 0, "accepted exact-case threshold (required)")
	p95 := flag.Int64("maximum-p95-ms", 0, "accepted p95 latency in milliseconds (required)")
	flag.Parse()
	if *precision <= 0 || *recall <= 0 || *f1 <= 0 || *exact <= 0 || *p95 <= 0 {
		fatal(fmt.Errorf("all quality and performance thresholds must be explicitly provided"))
	}
	f, err := os.Open(*datasetPath)
	fatal(err)
	defer f.Close()
	cases, digest, err := benchmark.Load(f)
	fatal(err)
	want, err := os.ReadFile(*digestPath)
	fatal(err)
	fatal(benchmark.VerifyGolden(digest, strings.Fields(string(want))[0]))
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := benchmark.Run(ctx, infrastructure.LlamaProvider{URL: *endpoint}, cases, digest, benchmark.Thresholds{
		MinimumPrecision: *precision, MinimumRecall: *recall, MinimumF1: *f1, MinimumExactRate: *exact, MaximumP95MS: *p95,
	})
	fatal(err)
	fatal(json.NewEncoder(os.Stdout).Encode(report))
	if !report.Passed {
		os.Exit(2)
	}
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
