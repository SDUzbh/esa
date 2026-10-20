package main

import (
	"context"
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

	"esa/internal/codex"
	"esa/internal/codexreview"
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
	registryPath := flag.String("registry", "", "Project/release registry for per-task historical source resolution")
	profiles := flag.String("profiles", "profiles", "Operator-approved versioned Profile and Skill directory")
	toolCatalog := flag.String("tool-catalog", "", "Private JSON catalog of approved pinned temporary tools")
	mavenRepository := flag.String("maven-repository", "https://repo1.maven.org/maven2",
		"HTTPS repository engineering may install any artifact from on demand (maven_install); empty disables it")
	mavenCache := flag.String("maven-cache", "runtime/maven-cache",
		"Host directory that keeps verified Maven downloads for all tasks, so each artifact is fetched once; empty disables it")
	mavenCacheMB := flag.Int64("maven-cache-max-mb", 2048, "Size cap of -maven-cache in MiB; least recently used files are removed first")
	mavenBeforeCutoff := flag.Bool("maven-before-cutoff", false,
		"Historical replays only: refuse artifacts published after each task's observation cutoff")
	concurrency := flag.Int("concurrency", 3, "Maximum engineering environments running simultaneously (1..3)")
	issuesRoot := flag.String("issues-directory", "runtime/esa/issues", "Local read-only root of ESA Issue projections shared with the service")
	runs := flag.String("runs", ".esa/codex-runs", "Private invocation audit directory")
	tokenFile := flag.String(
		"token-env-file",
		"deploy/redmine/.env",
		"Private environment file with ESA_ANALYZER_TOKEN, used when that variable is not set; only that key is read",
	)
	cli := flag.String("codex", "codex", "Installed Codex CLI")
	var acceptedFeatures []string
	flag.Func("accept-cli-feature", "Start even though the Codex CLI does not honour ESA's setting for this feature (repeatable; logged as a warning)", func(name string) error {
		acceptedFeatures = append(acceptedFeatures, name)
		return nil
	})
	model := flag.String("model", "", "Use the configured local model when unspecified")
	effort := flag.String("effort", "", "Use the configured local reasoning effort when unspecified")
	toolsConfig := flag.String("tools-config", "", "Operator-created isolated replay tool registration")
	toolServer := flag.Bool("tool-server", false, "Serve only the registered replay MCP tools over stdio")
	once := flag.Bool("once", false, "Diagnose one registered packet and exit; used by replay pipelines")
	runID := flag.String("run-id", "", "Unique diagnosis ID for once mode")
	rules := flag.String("rules", "config/rules.json", "Workflow budget and advisory token thresholds")
	managed := flag.Bool("managed-tools", true, "Prepare and clean task-local workspaces and explicitly registered replay scenarios")
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
	if *reviewFile == "" && *registryPath == "" {
		b, err = os.ReadFile(*evidence)
	}
	if err != nil {
		return err
	}
	var packet codex.Packet
	if *reviewFile == "" && *registryPath == "" {
		if err = json.Unmarshal(b, &packet); err != nil {
			return err
		}
	}
	if *registryPath != "" && (*once || *toolsConfig != "" || *reviewFile != "") {
		return fmt.Errorf("registry mode requires the HTTP provider with task-local tool registrations")
	}
	if *reviewFile == "" &&
		*registryPath == "" &&
		((len(packet.Documents) == 0 && *toolsConfig == "") || packet.Ticket.SourceKey == "" || packet.Cutoff.IsZero()) {
		return fmt.Errorf("invalid evidence packet")
	}
	// ESA authenticates to this provider with ESA_ANALYZER_TOKEN, a secret of
	// its own: the provider never needs ESA's API or outbox token.
	token := os.Getenv("ESA_ANALYZER_TOKEN")
	if token == "" && !*once && *reviewFile == "" {
		b, err = os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "ESA_ANALYZER_TOKEN=") {
				token = strings.Trim(strings.TrimPrefix(line, "ESA_ANALYZER_TOKEN="), "\"'")
			}
		}
	}
	if len(token) < 24 && !*once && *reviewFile == "" {
		return fmt.Errorf("missing analyzer token: set ESA_ANALYZER_TOKEN (at least 24 characters) or put it in -token-env-file")
	}
	path, err := exec.LookPath(*cli)
	if err != nil && *cli == "codex" {
		path = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
		_, err = os.Stat(path)
	}
	if err != nil {
		return err
	}
	check, cancelCheck := context.WithTimeout(context.Background(), 30*time.Second)
	warnings, err := codex.CheckCLIFeatures(check, path, acceptedFeatures)
	cancelCheck()
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		slog.Warn("Codex CLI feature not honoured; accepted by -accept-cli-feature", "feature", warning)
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
	profileDir, err := filepath.Abs(*profiles)
	if err != nil {
		return err
	}
	issueRoot, err := filepath.Abs(*issuesRoot)
	if err != nil {
		return err
	}
	provider := &codex.Provider{
		IssuesRoot:   issueRoot,
		CLI:          path,
		Model:        *model,
		Effort:       *effort,
		RunsDir:      dir,
		Packet:       packet,
		ToolsConfig:  *toolsConfig,
		ToolCommand:  toolCommand,
		ProfilesRoot: profileDir,
	}
	if *mavenRepository != "" && !strings.HasPrefix(*mavenRepository, "https://") {
		return fmt.Errorf("-maven-repository must be an HTTPS URL")
	}
	if *mavenBeforeCutoff && *mavenRepository == "" {
		return fmt.Errorf("-maven-before-cutoff needs -maven-repository")
	}
	provider.MavenRepository, provider.MavenBeforeCutoff = *mavenRepository, *mavenBeforeCutoff
	if *mavenCache != "" && *mavenRepository != "" {
		// The tool server runs from another directory; give it an absolute path.
		if provider.MavenCache, err = filepath.Abs(*mavenCache); err != nil {
			return err
		}
		if *mavenCacheMB < 1 {
			return fmt.Errorf("-maven-cache-max-mb must be positive")
		}
		provider.MavenCacheMaxBytes = *mavenCacheMB << 20
	}
	if *toolCatalog != "" {
		data, err := os.ReadFile(*toolCatalog)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &provider.ToolCatalog); err != nil {
			return err
		}
		if len(provider.ToolCatalog) > 64 {
			return fmt.Errorf("tool catalog must contain at most 64 packages")
		}
	}
	if *concurrency < 1 || *concurrency > 3 {
		return fmt.Errorf("engineering concurrency must be 1..3")
	}
	provider.Slots = make(chan struct{}, *concurrency)
	if *registryPath != "" {
		script := ""
		if *managed {
			script, err = filepath.Abs("scripts/replay_session.py")
			if err != nil {
				return err
			}
		}
		registry, err := codex.LoadRegistry(*registryPath, filepath.Join(dir, "source-cache"), profileDir, script)
		if err != nil {
			return err
		}
		provider.Resolve = registry.Resolve
		provider.CheckRelease = registry.CheckRelease
	}
	if *reviewFile != "" {
		if *toolsConfig != "" {
			return fmt.Errorf("review mode must not expose diagnostic tools")
		}
		b, err = os.ReadFile(*reviewFile)
		if err != nil {
			return err
		}
		var request codexreview.ReviewRequest
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
	if *managed && *toolsConfig == "" && *registryPath == "" {
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
		if err = esa.ValidateConfig(config); err != nil {
			return err
		}
		if *runID == "" {
			*runID = fmt.Sprintf("run_replay_%d", time.Now().UnixNano())
		}
		in := esa.AnalyzeRequest{RunID: *runID, Ticket: packet.Ticket, Budget: config.Budget, ReadOnly: true}
		report, err := provider.Analyze(ctx, in)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           provider.Handler(token),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      16 * time.Minute,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}
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
