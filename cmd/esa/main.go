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
	data := flag.String("store", "runtime/esa/state.json", "Single-process durable state file; not used with -database-url")
	issueDirectory := flag.String("issues-directory", "", "Local Issue projection root shared read-only with the provider")
	databaseURL := flag.String("database-url", os.Getenv("ESA_DATABASE_URL"), "ESA-owned Postgres state database; replaces the JSON -store")
	stateURL := flag.String("state-url", os.Getenv("ESA_STATE_URL"), "Deprecated: Redmine state API as the authority; use -database-url")
	importState := flag.String("import-state", "", "Import a stopped JSON store into an empty -database-url (or -state-url) state and exit")
	importStateURL := flag.String("import-state-url", "", "With -database-url: import a Redmine state API's state into the empty database and exit")
	rules := flag.String("rules", "config/rules.json", "Workflow configuration and analysis budget")
	replayAt := flag.String("replay-at", "", "Test-only observation clock initial time (RFC3339)")
	replayProjects := flag.String("replay-projects", "", "Comma-separated operator project scope for the test clock")
	health := flag.Bool("healthcheck", false, "Check the local ESA HTTP server and exit")
	flag.Parse()
	if *health {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8090/healthz")
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
	if *databaseURL != "" && *stateURL != "" {
		return fmt.Errorf("-database-url and -state-url are mutually exclusive")
	}
	if *importStateURL != "" && *databaseURL == "" {
		return fmt.Errorf("-import-state-url requires -database-url")
	}
	if *databaseURL != "" && (*importState != "" || *importStateURL != "") {
		return importIntoPostgres(*databaseURL, *importState, *importStateURL, token)
	}
	if *stateURL != "" && *importState != "" {
		backend := esa.HTTPStateBackend{URL: *stateURL, Token: token}
		if err := backend.Validate(); err != nil {
			return err
		}
		return importIntoBackend(backend, *importState)
	}
	if *stateURL == "" && *databaseURL == "" {
		unlock, err := lockJSONStore(*data)
		if err != nil {
			return err
		}
		defer unlock()
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

	var store *esa.Store
	switch {
	case *databaseURL != "":
		persister, err := esa.OpenPostgres(runCtx, *databaseURL, esa.PostgresOptions{})
		if err != nil {
			return err
		}
		defer persister.Close()
		slog.Info("ESA standby: waiting for the state writer role", "address", *address)
		if store, err = esa.OpenPostgresStore(runCtx, persister, 5*time.Second); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return firstError(serverErr, err)
		}
		store.OnLostWriter(lose)
		go persister.WatchLock(runCtx, 5*time.Second, lose)
	case *stateURL != "":
		slog.Warn("-state-url is deprecated; migrate to -database-url")
		backend := esa.HTTPStateBackend{URL: *stateURL, Token: token}
		if err = backend.Validate(); err != nil {
			return err
		}
		store, err = esa.OpenBackendStore(backend)
	default:
		if *importState != "" {
			return fmt.Errorf("state import requires -database-url")
		}
		store, err = esa.OpenStore(*data)
	}
	if err != nil {
		return err
	}
	engine, err := esa.NewEngine(store, config)
	if err != nil {
		return err
	}
	if *issueDirectory == "" {
		*issueDirectory = filepath.Join(filepath.Dir(*data), "issues")
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
	slog.Info("ESA started", "address", *address, "workflow_version", engine.Version(), "analyzer_configured", analyzer != nil, "postgres", *databaseURL != "")
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

// lockJSONStore takes the single-writer file lock next to a JSON store.
func lockJSONStore(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("store already has a writer: %w", err)
	}
	return func() { lock.Close() }, nil
}

// importIntoPostgres imports a stopped JSON store or a Redmine state API into
// an empty ESA database, verifies it in the same transaction, and exits. The
// source is never modified.
func importIntoPostgres(databaseURL, jsonPath, stateURL, token string) error {
	if (jsonPath == "") == (stateURL == "") {
		return fmt.Errorf("import needs exactly one of -import-state or -import-state-url")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var source *esa.Store
	var err error
	if jsonPath != "" {
		if _, err = os.Stat(jsonPath); err != nil {
			return err
		}
		unlock, err := lockJSONStore(jsonPath)
		if err != nil {
			return fmt.Errorf("stop the ESA writer before importing: %w", err)
		}
		defer unlock()
		source, err = esa.OpenStore(jsonPath)
		if err != nil {
			return err
		}
	} else {
		backend := esa.HTTPStateBackend{URL: stateURL, Token: token}
		if err = backend.Validate(); err != nil {
			return err
		}
		if source, err = esa.OpenBackendStore(backend); err != nil {
			return err
		}
	}
	persister, err := esa.OpenPostgres(ctx, databaseURL, esa.PostgresOptions{})
	if err != nil {
		return err
	}
	defer persister.Close()
	wait, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = persister.AcquireWriter(wait, time.Second); err != nil {
		return fmt.Errorf("another ESA process holds the state writer role; stop it before importing: %w", err)
	}
	snapshot := source.Snapshot()
	if err = persister.Import(ctx, snapshot); err != nil {
		return err
	}
	slog.Info("State imported and verified; the source was not modified", "tickets", len(snapshot.Tickets), "events", len(snapshot.Events), "snapshots", len(snapshot.Snapshots))
	return nil
}

// importIntoBackend keeps the deprecated JSON-to-Redmine-state import.
func importIntoBackend(backend esa.HTTPStateBackend, jsonPath string) error {
	state, revision, err := backend.Load()
	if err != nil {
		return err
	}
	if revision != 0 || len(state.Tickets) != 0 || len(state.Snapshots) != 0 || len(state.Investigations) != 0 {
		return fmt.Errorf("state import requires a pristine backend")
	}
	legacy, err := esa.OpenStore(jsonPath)
	if err != nil {
		return err
	}
	if _, err = backend.Commit(legacy.Snapshot(), 0); err != nil {
		return err
	}
	slog.Info("State imported; original JSON file preserved")
	return nil
}
