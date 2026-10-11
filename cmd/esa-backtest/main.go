// esa-backtest tracks version graphs and validates retrospective Agent reviews.
package main

import (
	"context"
	"encoding/json"
	"esa/internal/backtest"
	"esa/internal/codex"
	"esa/internal/esa"
	"esa/internal/replay"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

func read(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "track", "track, packet, evaluate, assign or export")
	repo := flag.String("repo", "kafka", "Read-only product repository")
	mainline := flag.String("mainline", "origin/trunk", "Main branch to track")
	baseline := flag.String("baseline", "", "Frozen diagnosis commit")
	cutoff := flag.String("cutoff", "", "Ticket observation RFC3339 timestamp")
	until := flag.String("until", "", "Optional evaluation end RFC3339 timestamp")
	versions := flag.Int("versions", 5, "Number of subsequent mainline version observation points")
	issuePath := flag.String("issue", "", "Issue JSON")
	tracePath := flag.String("trace", "", "Trace JSON")
	predictionPath := flag.String("prediction", "", "Frozen engineering report JSON")
	ticketPath := flag.String("ticket", "", "Frozen ticket JSON")
	registryPath := flag.String("registry", "", "Issue registry JSON list")
	commit := flag.String("commit", "", "Candidate or export commit")
	reviewsPath := flag.String("reviews", "", "Review list JSON")
	sourceKey := flag.String("source-key", "", "Ticket source key")
	out := flag.String("out", "", "New export directory")
	packets := flag.String("packets", "", "Saved candidate review packets including runtime documents")
	scope := flag.String("scope", "full", "Export full or clients source")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	git := backtest.Git{Repo: *repo, Context: ctx}
	var result any
	switch *mode {
	case "validate":
		var prediction esa.Report
		if e := read(*predictionPath, &prediction); e != nil {
			return e
		}
		if prediction.AgentClassification == nil || prediction.Issue == nil {
			return fmt.Errorf("structured classification and issue are required")
		}
		packet := codex.Packet{Commit: *baseline}
		if prediction.Issue.ExistingID != "" {
			packet.Issues = []esa.IssueBrief{{ID: prediction.Issue.ExistingID, Category: prediction.AgentClassification.Category}}
		}
		findings := append([]esa.Finding(nil), prediction.Findings...)
		for i, f := range findings {
			b, e := execGit(ctx, *repo, "show", *baseline+":"+f.File)
			if e != nil {
				return e
			}
			lines := strings.Split(string(b), "\n")
			packet.Documents = append(packet.Documents, codex.Document{Source: "git", Prefix: *baseline + ":" + f.File + ":", First: 1, Lines: lines})
			findings[i].Evidence = nil
			for _, ev := range f.Evidence {
				if ev.Source == "git" {
					findings[i].Evidence = append(findings[i].Evidence, ev)
				}
			}
		}
		out := codex.EngineeringOutput{Classification: prediction.AgentClassification.Category, Issue: *prediction.Issue, Findings: findings, Limitations: prediction.Limitations}
		if _, e := out.Report(packet); e != nil {
			return e
		}
		result = map[string]string{"state": "validated", "scope": "Historical source localization; external runtime evidence and input provenance remain operator supplied."}
	case "assign":
		var registry []backtest.Issue
		var ticket esa.Ticket
		var prediction esa.Report
		if e := read(*registryPath, &registry); e != nil {
			return e
		}
		if e := read(*ticketPath, &ticket); e != nil {
			return e
		}
		if e := read(*predictionPath, &prediction); e != nil {
			return e
		}
		issues, issue, e := backtest.Assign(registry, ticket, prediction)
		if e != nil {
			return e
		}
		result = map[string]any{"issues": issues, "issue": issue}
	case "track":
		var issue backtest.Issue
		if e := read(*issuePath, &issue); e != nil {
			return e
		}
		r, e := git.Trace(issue, *baseline, *mainline, *cutoff, *until, *versions)
		if e != nil {
			return e
		}
		result = r
	case "packet", "evaluate":
		var trace backtest.Trace
		var prediction esa.Report
		if e := read(*tracePath, &trace); e != nil {
			return e
		}
		if e := read(*predictionPath, &prediction); e != nil {
			return e
		}
		if *mode == "packet" {
			found := false
			for _, c := range trace.Candidates {
				if c.Commit == *commit {
					r, e := backtest.ReviewPacket(git, trace, prediction, *sourceKey, c)
					if e != nil {
						return e
					}
					result = r
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("candidate not found in frozen trace")
			}
		} else {
			var reviews []backtest.Review
			if e := read(*reviewsPath, &reviews); e != nil {
				return e
			}
			for _, r := range reviews {
				found := false
				for _, c := range trace.Candidates {
					if c.Commit == r.Commit {
						packet, e := backtest.ReviewPacket(git, trace, prediction, *sourceKey, c)
						if e != nil {
							return e
						}
						if *packets != "" {
							var saved backtest.ReviewRequest
							if e = read(filepath.Join(*packets, c.Commit, "packet.json"), &saved); e != nil {
								return e
							}
							if !reflect.DeepEqual(saved.Candidate, packet.Candidate) || !reflect.DeepEqual(saved.Prediction, packet.Prediction) || saved.Baseline != packet.Baseline {
								return fmt.Errorf("saved review packet differs from registered prediction or candidate")
							}
							if len(saved.Documents) < len(packet.Documents) || !reflect.DeepEqual(saved.Documents[:len(packet.Documents)], packet.Documents) {
								return fmt.Errorf("saved Git review evidence changed")
							}
							packet = saved
						}
						if e = backtest.ValidateReview(r, packet); e != nil {
							return e
						}
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("review commit is absent from trace")
				}
			}
			r, e := backtest.Evaluate(*sourceKey, trace, prediction, reviews)
			if e != nil {
				return e
			}
			result = r
		}
	case "export":
		// Future snapshots are exported only through this evaluation entry point.
		// They are never accepted by the historical diagnosis resolver.
		var trace backtest.Trace
		if e := read(*tracePath, &trace); e != nil {
			return e
		}
		allowed := false
		for _, c := range trace.Candidates {
			if c.Commit == *commit || c.Parent == *commit {
				allowed = true
			}
		}
		for _, v := range trace.Versions {
			if v.MainlineCommit == *commit {
				allowed = true
			}
		}
		if !allowed {
			return fmt.Errorf("export revision is absent from registered trace")
		}
		p := replay.Plan{Commit: *commit, Ref: *commit}
		if e := replay.Export(ctx, p, *repo, *out, *scope); e != nil {
			return e
		}
		result = map[string]any{"directory": *out, "source_commit": *commit}
	default:
		return fmt.Errorf("unknown mode")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func execGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...).Output()
}
