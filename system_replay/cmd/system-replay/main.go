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
	flag.Parse()
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
