package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	data := flag.String("store", "runtime/esa/state.json", "Single-process durable state file")
	stateURL := flag.String("state-url", os.Getenv("ESA_STATE_URL"), "Authoritative Redmine state API; disables the local JSON writer")
	importState := flag.String("import-state", "", "Import a stopped JSON store into an empty authoritative backend and exit")
	rules := flag.String("rules", "config/rules.json", "Workflow configuration and analysis budget")
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
	if *stateURL == "" {
		if err := os.MkdirAll(filepath.Dir(*data), 0700); err != nil {
			return err
		}
		lock, err := os.OpenFile(*data+".lock", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fmt.Errorf("store already has a writer: %w", err)
		}
	}
	b, err := os.ReadFile(*rules)
	if err != nil {
		return err
	}
	var config esa.Config
	if err := json.Unmarshal(b, &config); err != nil {
		return err
	}
	var store *esa.Store
	if *stateURL != "" {
		backend := esa.HTTPStateBackend{URL: *stateURL, Token: token}
		if err = backend.Validate(); err != nil {
			return err
		}
		store, err = esa.OpenBackendStore(backend)
		if err == nil && *importState != "" {
			state, revision, loadErr := backend.Load()
			if loadErr != nil {
				return loadErr
			}
			if revision != 0 || len(state.Tickets) != 0 || len(state.Snapshots) != 0 || len(state.Investigations) != 0 {
				return fmt.Errorf("state import requires a pristine backend")
			}
			legacy, loadErr := esa.OpenStore(*importState)
			if loadErr != nil {
				return loadErr
			}
			if _, loadErr = backend.Commit(legacy.Snapshot(), 0); loadErr != nil {
				return loadErr
			}
			slog.Info("State imported; original JSON file preserved")
			return nil
		}
	} else {
		if *importState != "" {
			return fmt.Errorf("state import requires -state-url")
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
	if err = engine.Recover(); err != nil {
		return err
	}
	var analyzer esa.Analyzer
	if url := os.Getenv("ESA_ANALYZER_URL"); url != "" {
		analyzer = esa.HTTPAnalyzer{URL: url, Token: os.Getenv("ESA_ANALYZER_TOKEN")}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *address, Handler: (&esa.API{Engine: engine, Token: token}).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := engine.SyncRedmine(ctx, os.Getenv("ESA_REDMINE_URL"), token, &http.Client{Timeout: 15 * time.Second}); err != nil {
				slog.Warn("Redmine synchronization failed", "error", err)
			}
		}
	}()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := engine.Refresh(); err != nil {
				slog.Warn("State refresh failed", "error", err)
				continue
			}
			if _, err := engine.ProcessOne(ctx, analyzer); err != nil {
				slog.Error("Workflow processing failed", "error", err)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("ESA started", "address", *address, "workflow_version", engine.Version(), "analyzer_configured", analyzer != nil)
	err = server.ListenAndServe()
	stop()
	workers.Wait()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
