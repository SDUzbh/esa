package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"systemreplay"
)

func main() {
	manifest := flag.String("manifest", "", "Independent system replay manifest")
	out := flag.String("output", "", "New output directory")
	plan := flag.Bool("plan", false, "Validate historical inputs without executing systems")
	pipeline := flag.Bool("pipeline", false, "Replay a batch with runtime differences")
	attach := flag.Bool("attach", false, "Explicitly use an existing target; managed fresh process is the default")
	track := flag.Bool("track-only", false, "Inspect Git without target calls or model calls")
	order := flag.String("order", "version", "Initial order: version or time")
	score := flag.String("score", "", "Score a finished run's pipeline.json against -labels and write -output (no target or model calls)")
	labels := flag.String("labels", "", "Evaluation labels for -score")
	repo := flag.String("repo", "", "Product checkout the run used, for -score")
	flag.Parse()
	if *score != "" {
		os.Exit(scoreRun(*score, *labels, *repo, *out))
	}
	if *manifest == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "--manifest and --output are required")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	summary, e := systemreplay.Run(ctx, *manifest, *out, systemreplay.Options{PlanOnly: *plan, Pipeline: *pipeline, Attach: *attach, TrackOnly: *track, Order: *order})
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"report": *out + "/pipeline.json", "phase": summary.Phase, "cleanup": summary.Cleanup})
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func scoreRun(summaryPath, labelsPath, repo, out string) int {
	if labelsPath == "" || repo == "" || out == "" {
		fmt.Fprintln(os.Stderr, "-score needs -labels, -repo and a new -output directory")
		return 2
	}
	var summary systemreplay.Summary
	var labels systemreplay.Labels
	for path, value := range map[string]any{summaryPath: &summary, labelsPath: &labels} {
		raw, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(raw, value)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, path+":", err)
			return 1
		}
	}
	report := systemreplay.ScoreRun(context.Background(), summary, labels, repo)
	if err := systemreplay.WriteScore(out, report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"report": out + "/report.md", "tickets": len(report.Tickets),
		"routing_accuracy": report.Routing.Value, "file_hit": report.FileHit.Value,
	})
	return 0
}
