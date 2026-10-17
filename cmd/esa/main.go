package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"esa/internal/esa"
)

func main() {
	if err := run(); err != nil {
		slog.Error("ESA stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	address := flag.String("listen", "127.0.0.1:8090", "HTTP listen address")
	issueDirectory := flag.String("issues-directory", "runtime/esa/issues", "Local Issue projection root shared read-only with the provider")
	databaseURL := flag.String("database-url", os.Getenv("ESA_DATABASE_URL"), "ESA-owned Postgres state database (required)")
	stateURL := flag.String("state-url", os.Getenv("ESA_STATE_URL"), "Removed: the Redmine state API is no longer supported; ESA refuses to start while it is set")
	ephemeral := flag.Bool("ephemeral-schema", false, "Test-only: keep the state in a new schema that is dropped on exit")
	rules := flag.String("rules", "config/rules.json", "Workflow configuration and analysis budget")
	replayAt := flag.String("replay-at", "", "Test-only observation clock initial time (RFC3339)")
	replayProjects := flag.String("replay-projects", "", "Comma-separated operator project scope for the test clock")
	health := flag.Bool("healthcheck", false, "Check that the local ESA HTTP server is the active writer and exit")
	flag.Parse()
	if *health {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8090/healthz?role=active")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("healthcheck returned HTTP %d", resp.StatusCode)
		}
		return nil
	}
	token := os.Getenv("ESA_API_TOKEN")
	if len(token) < 24 {
		return fmt.Errorf("ESA_API_TOKEN must contain at least 24 characters")
	}
	if err := checkStoreFlags(*databaseURL, *stateURL); err != nil {
		return err
	}
	b, err := os.ReadFile(*rules)
	if err != nil {
		return err
	}
	var config esa.Config
	if err := json.Unmarshal(b, &config); err != nil {
		return err
	}
	if *replayAt != "" || *replayProjects != "" {
		at, err := time.Parse(time.RFC3339Nano, *replayAt)
		if err != nil {
			return fmt.Errorf("invalid -replay-at: %w", err)
		}
		projects := []string{}
		for _, project := range strings.Split(*replayProjects, ",") {
			if project = strings.TrimSpace(project); project != "" {
				projects = append(projects, project)
			}
		}
		config.Replay = esa.ReplayConfig{Enabled: true, InitialTime: at, Projects: projects}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	// The server answers as a standby until this process holds the state and
	// the API is activated; after a lost writer role it answers 503.
	gate := &esa.WriterGate{}
	server := &http.Server{Addr: *address, Handler: gate, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
		cancelRun()
	}()
	go func() {
		<-runCtx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	lost := make(chan error, 1)
	lose := func(err error) {
		select {
		case lost <- err:
			gate.Lose()
			slog.Error("Lost the state writer role; stopping", "error", err)
			cancelRun()
		default:
		}
	}

	options := esa.PostgresOptions{}
	if *ephemeral {
		options.Schema = esa.EphemeralSchema()
	}
	persister, err := esa.OpenPostgres(runCtx, *databaseURL, options)
	if err != nil {
		return err
	}
	defer persister.Close()
	if *ephemeral {
		defer func() {
			drop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := persister.DropSchema(drop); err != nil {
				slog.Warn("Could not drop the ephemeral schema", "error", err)
			}
		}()
	}
	slog.Info("ESA standby: waiting for the state writer role", "address", *address)
	store, err := esa.OpenPostgresStore(runCtx, persister, 5*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return firstError(serverErr, err)
	}
	store.OnLostWriter(lose)
	go persister.WatchLock(runCtx, 5*time.Second, lose)
	engine, err := esa.NewEngine(store, config)
	if err != nil {
		return err
	}
	engine.IssueDirectory, err = filepath.Abs(*issueDirectory)
	if err != nil {
		return err
	}
	if err = engine.Recover(); err != nil {
		return err
	}
	var analyzer esa.Analyzer
	if url := os.Getenv("ESA_ANALYZER_URL"); url != "" {
		analyzer = esa.HTTPPipelineAnalyzer{HTTPAnalyzer: esa.HTTPAnalyzer{URL: url, Token: os.Getenv("ESA_ANALYZER_TOKEN")}}
	}
	var workers sync.WaitGroup
	workers.Add(1 + esa.EngineeringConcurrency + 1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			if err := engine.SyncRedmine(runCtx, os.Getenv("ESA_REDMINE_URL"), token, &http.Client{Timeout: 15 * time.Second}); err != nil {
				slog.Warn("Redmine synchronization failed", "error", err)
			}
		}
	}()
	for worker := 0; worker < esa.EngineeringConcurrency+1; worker++ {
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if runCtx.Err() != nil {
					return
				}
				if err := engine.Refresh(); err != nil {
					slog.Warn("State refresh failed", "error", err)
					select {
					case <-runCtx.Done():
						return
					case <-ticker.C:
					}
					continue
				}
				// Take the signal before looking for work so a slot freed meanwhile
				// is not missed. The ticker is the slow fallback for other processes,
				// queue-wait limits and lease expiry.
				wake := engine.WorkSignal()
				worked, err := engine.ProcessOne(runCtx, analyzer)
				if err != nil {
					slog.Error("Workflow processing failed", "error", err)
				}
				if worked && err == nil {
					continue
				}
				select {
				case <-runCtx.Done():
					return
				case <-wake:
				case <-ticker.C:
				}
			}
		}()
	}
	gate.Activate((&esa.API{Engine: engine, Token: token}).Handler())
	slog.Info("ESA started", "address", *address, "workflow_version", engine.Version(), "analyzer_configured", analyzer != nil)
	err = <-serverErr
	cancelRun()
	workers.Wait()
	select {
	case lostErr := <-lost:
		return lostErr
	default:
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// checkStoreFlags requires the Postgres store. The removed Redmine state API
// (-state-url or ESA_STATE_URL) stops ESA instead of being ignored.
func checkStoreFlags(databaseURL, stateURL string) error {
	if stateURL != "" {
		return fmt.Errorf("the Redmine state API (-state-url or ESA_STATE_URL) is no longer supported; unset it and set -database-url, see docs/ESA_OPERATIONS.md#postgres-state-store")
	}
	if databaseURL == "" {
		return fmt.Errorf("ESA keeps its state in Postgres: set ESA_DATABASE_URL or -database-url, see docs/ESA_OPERATIONS.md#postgres-state-store")
	}
	return nil
}

// firstError prefers a server failure (such as a busy port) that ended the wait.
func firstError(serverErr <-chan error, err error) error {
	select {
	case serr := <-serverErr:
		if !errors.Is(serr, http.ErrServerClosed) {
			return serr
		}
	default:
	}
	return err
}
