// esa-replay resolves historical versions and exports immutable source snapshots.
package main

import (
	"context"
	"encoding/json"
	"esa/internal/replay"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	raw := flag.String("raw", "", "Preserved Jira JSON export")
	repo := flag.String("repo", "kafka", "Read-only Git repository")
	ref := flag.String("ref", "", "Optional historical ref; otherwise use the single affected version tag")
	at := flag.String("at", "", "Historical UTC cutoff; defaults to original creation")
	out := flag.String("out", "", "New replay bundle directory")
	project := flag.String("project", "1", "Redmine project scope")
	environment := flag.String("environment", "", "Explicit reported scenario supplement, JSON")
	scope := flag.String("scope", "clients", "Export clients or full historical source")
	planOnly := flag.Bool("plan", false, "Resolve and output a plan without preparing an environment")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p, err := replay.Resolve(ctx, *raw, *repo, *ref, *at, *project)
	if err != nil {
		return err
	}
	if *environment != "" {
		if err = json.Unmarshal([]byte(*environment), &p.Ticket.Environment); err != nil {
			return err
		}
	}
	if *planOnly {
		return json.NewEncoder(os.Stdout).Encode(p)
	}
	if err = replay.Export(ctx, p, *repo, *out, *scope); err != nil {
		return err
	}
	fmt.Printf("Prepared %s at %s from %s\n", p.Ticket.SourceKey, p.Cutoff.Format("2006-01-02T15:04:05.999999999Z07:00"), p.Commit)
	return nil
}
