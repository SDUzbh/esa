package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"esa/internal/backtest"
	"esa/internal/codex"
	"esa/internal/esa"
	"esa/internal/replaytools"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Codex adapter stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	reviewFile := flag.String("review", "", "Retrospective review packet; runs a separate tool-free Agent")
	listen := flag.String("listen", "127.0.0.1:8091", "Local provider listen address")
	evidence := flag.String("evidence", "", "Registered immutable replay evidence packet")
	runs := flag.String("runs", ".esa/codex-runs", "Private invocation audit directory")
	tokenFile := flag.String("token-env-file", "deploy/redmine/.env", "Private environment file containing ESA_API_TOKEN")
	cli := flag.String("codex", "codex", "Installed Codex CLI")
	model := flag.String("model", "", "Use the configured local model when unspecified")
	effort := flag.String("effort", "", "Use the configured local reasoning effort when unspecified")
	toolsConfig := flag.String("tools-config", "", "Operator-created isolated replay tool registration")
	toolServer := flag.Bool("tool-server", false, "Serve only the registered replay MCP tools over stdio")
	once := flag.Bool("once", false, "Diagnose one registered packet and exit; used by replay pipelines")
	runID := flag.String("run-id", "", "Unique diagnosis ID for once mode")
	rules := flag.String("rules", "config/rules.json", "Classification and advisory token thresholds")
	managed := flag.Bool("managed-tools", true, "Automatically prepare and clean the registered packet's replay environment")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *toolServer {
		config, err := replaytools.Load(*toolsConfig)
		if err != nil {
			return err
		}
		return (&replaytools.Server{Config: config}).Serve(ctx, os.Stdin, os.Stdout)
	}
	var b []byte
	var err error
	if *reviewFile == "" {
		b, err = os.ReadFile(*evidence)
	}
	if err != nil {
		return err
	}
	var packet codex.Packet
	if *reviewFile == "" {
		if err = json.Unmarshal(b, &packet); err != nil {
			return err
		}
	}
	if *reviewFile == "" && ((len(packet.Documents) == 0 && *toolsConfig == "") || packet.Ticket.SourceKey == "" || packet.Cutoff.IsZero()) {
		return fmt.Errorf("invalid evidence packet")
	}
	token := ""
	if !*once && *reviewFile == "" {
		b, err = os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "ESA_API_TOKEN=") {
				token = strings.Trim(strings.TrimPrefix(line, "ESA_API_TOKEN="), "\"'")
			}
		}
		if len(token) < 24 {
			return fmt.Errorf("missing provider service token")
		}
	}
	path, err := exec.LookPath(*cli)
	if err != nil && *cli == "codex" {
		path = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
		_, err = os.Stat(path)
	}
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(*runs)
	if err != nil {
		return err
	}
	// Read only explicit model preferences; do not inherit MCPs, hooks or user prompts.
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	prefs, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	for _, line := range strings.Split(string(prefs), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), "\"")
		switch strings.TrimSpace(k) {
		case "model":
			if *model == "" {
				*model = v
			}
		case "model_reasoning_effort":
			if *effort == "" {
				*effort = v
			}
		}
	}
	toolCommand, err := os.Executable()
	if err != nil {
		return err
	}
	if *toolsConfig != "" {
		*toolsConfig, err = filepath.Abs(*toolsConfig)
		if err != nil {
			return err
		}
	}
	provider := &codex.Provider{CLI: path, Model: *model, Effort: *effort, RunsDir: dir, Packet: packet, ToolsConfig: *toolsConfig, ToolCommand: toolCommand}
	if *reviewFile != "" {
		if *toolsConfig != "" {
			return fmt.Errorf("review mode must not expose diagnostic tools")
		}
		b, err = os.ReadFile(*reviewFile)
		if err != nil {
			return err
		}
		var request backtest.ReviewRequest
		if err = json.Unmarshal(b, &request); err != nil {
			return err
		}
		b, err = os.ReadFile(*rules)
		if err != nil {
			return err
		}
		var config esa.Config
		if err = json.Unmarshal(b, &config); err != nil {
			return err
		}
		if *runID == "" {
			*runID = fmt.Sprintf("run_review_%d", time.Now().UnixNano())
		}
		r, err := provider.Review(ctx, request, config.Budget, *runID)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	if *managed && *toolsConfig == "" {
		packetPath, err := filepath.Abs(*evidence)
		if err != nil {
			return err
		}
		if _, err = os.Stat(filepath.Join(filepath.Dir(packetPath), "manifest.json")); err != nil {
			return fmt.Errorf("managed replay requires the source manifest beside evidence.json; use -managed-tools=false for a frozen-report-only session")
		}
		script, err := filepath.Abs("scripts/replay_session.py")
		if err != nil {
			return err
		}
		provider.SetupEnvironment = codex.ManagedEnvironment(script, packetPath)
	}
	if *once {
		b, err = os.ReadFile(*rules)
		if err != nil {
			return err
		}
		var config esa.Config
		if err = json.Unmarshal(b, &config); err != nil {
			return err
		}
		store, _ := esa.OpenStore("")
		engine, err := esa.NewEngine(store, config)
		if err != nil {
			return err
		}
		if *runID == "" {
			*runID = fmt.Sprintf("run_replay_%d", time.Now().UnixNano())
		}
		in := esa.AnalyzeRequest{RunID: *runID, Ticket: packet.Ticket, Classification: engine.Classify(packet.Ticket), Budget: config.Budget, ReadOnly: true}
		report, err := provider.Analyze(ctx, in)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "provider": "codex_cli", "registered_source": packet.Ticket.SourceKey, "model": *model})
	})
	mux.HandleFunc("POST /analyze", func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "Authentication required", 401)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		var in esa.AnalyzeRequest
		if err := d.Decode(&in); err != nil {
			http.Error(w, "Invalid provider input", 400)
			return
		}
		report, err := provider.Analyze(r.Context(), in)
		if err != nil {
			slog.Warn("Codex diagnosis failed", "run_id", in.RunID, "error", err)
			http.Error(w, err.Error(), 422)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	})
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 16 * time.Minute, BaseContext: func(_ net.Listener) context.Context { return ctx }}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	slog.Info("Codex adapter started", "address", *listen, "registered_source", packet.Ticket.SourceKey, "model", *model)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	<-shutdownDone
	return nil
}
