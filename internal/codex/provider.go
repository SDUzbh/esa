// Package codex adapts the installed Codex CLI to ESA's read-only HTTP contract.
// Host tools are disabled. A registered MCP bridge can expose an isolated replay
// container and frozen source without granting product-source write access.
package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"esa/internal/capabilities"
	"esa/internal/esa"
	"esa/internal/replaytools"
)

type Document = replaytools.Document
type Packet struct {
	Cutoff      time.Time  `json:"cutoff"`
	Commit      string     `json:"source_commit"`
	Ticket      esa.Ticket `json:"expected_ticket"`
	Documents   []Document `json:"documents"`
	Limitations []string   `json:"limitations"`
}

// Provider owns bounded Codex invocations, per-run audits and registered tool access.
// Native host tools are disabled; remote HTTP providers require their own isolation policy.
type Provider struct {
	IssuesRoot                  string
	ToolNames                   []string
	CLI, Model, Effort, RunsDir string
	ToolsConfig, ToolCommand    string
	ProfilesRoot                string
	CheckRelease                func(esa.AnalyzeRequest) string
	Resolve                     func(context.Context, esa.AnalyzeRequest) (Registration, error)
	Slots                       chan struct{}
	ToolCatalog                 map[string]replaytools.ToolPackage
	SetupEnvironment            func(context.Context, string) (string, func() error, error)
	Packet                      Packet
	mu                          sync.Mutex
	workflows                   sync.Map
	slotsOnce                   sync.Once
}

// Fingerprint excludes live issue IDs/timestamps but preserves all diagnostic
// content and the project boundary. A later edit requires a new evidence packet.
func Fingerprint(t esa.Ticket) string {
	// Redmine stores description text with CRLF. Normalize only line endings;
	// changes to words, code, comments or diagnostic metadata still invalidate it.
	normalize := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n") }
	t.Subject = normalize(t.Subject)
	t.Description = normalize(t.Description)
	t.Comments = append([]esa.Comment(nil), t.Comments...)
	for i := range t.Comments {
		t.Comments[i].Body = normalize(t.Comments[i].Body)
	}
	b, _ := json.Marshal(struct {
		Key, Project, Subject, Description, Tracker string
		Components, Versions, Errors                []string
		Environment                                 map[string]string
		Comments                                    []esa.Comment
	}{t.SourceKey, t.ProjectID, t.Subject, t.Description, t.Tracker, t.Components, t.Versions, t.ErrorCodes, t.Environment, t.Comments})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ValidateEvidence checks exact supplied source/log references and quotations.
// Passing validates citation provenance, not root cause or repair correctness.
func ValidateEvidence(r esa.Report, p Packet) error {
	if r.Summary == "" || len(r.RootCauseCandidates) == 0 || len(r.Evidence) == 0 || len(r.Recommendations) == 0 || len(r.VerificationSteps) == 0 {
		return errors.New("diagnosis is missing required report content")
	}
	for _, ev := range r.Evidence {
		valid := false
		for _, d := range p.Documents {
			if ev.Source != d.Source || !strings.HasPrefix(ev.Reference, d.Prefix) {
				continue
			}
			line, err := strconv.Atoi(strings.TrimPrefix(ev.Reference, d.Prefix))
			index := line - d.First
			if err == nil && index >= 0 && index < len(d.Lines) && strings.TrimSpace(ev.Quote) != "" && strings.Contains(d.Lines[index], ev.Quote) {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("evidence quote does not match a supplied historical source line: %s", ev.Reference)
		}
	}
	return nil
}

// CLIArgs keeps tools, user instructions, MCPs, plugins and persisted history out
// of the diagnostic session. Login remains in the user's existing CLI auth store.
// CLIArgs constructs an isolated invocation without inherited tools, hooks or session reuse.
func CLIArgs(model, effort, dir string) []string {
	args := []string{
		"--no-daemon",
		"-a",
		"never",
		"exec",
		"--ignore-user-config",
		"--ignore-rules",
		"--ephemeral",
		"--skip-git-repo-check",
		"--sandbox",
		"read-only",
		"--color",
		"never",
		"--json",
		"-C",
		dir,
		"--output-schema",
		filepath.Join(dir, "schema.json"),
		"-o",
		filepath.Join(dir, "answer.json"),
		"-c",
		"web_search=\"disabled\"",
		"-c",
		"project_doc_max_bytes=0",
		"-c",
		"model_instructions_file=" + strconv.Quote(filepath.Join(dir, "instructions.txt")),
	}
	// unified_exec (resumable command execution) is not listed: the CLI keeps it
	// on, since only managed requirements can disable it. Command execution is
	// kept out by shell_tool=false, the read-only sandbox, and the event check
	// in invoke that cancels a run on any command item.
	for _, feature := range []string{
		"shell_tool",
		"apps",
		"plugins",
		"hooks",
		"browser_use",
		"browser_use_external",
		"computer_use",
		"multi_agent",
		"code_mode",
		"code_mode_host",
		"view_image",
		"workspace_dependencies",
		"skill_search",
		"goals",
		"sleep_tool",
	} {
		args = append(args, "-c", "features."+feature+"=false")
	}
	args = append(args, "-c", "features.skip_host_skill_discovery=true")
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(effort))
	}
	return append(args, "-")
}

func (p *Provider) cliArgs(dir string) []string {
	args := CLIArgs(p.Model, p.Effort, dir)
	if p.ToolsConfig == "" {
		return args
	}
	args = args[:len(args)-1]
	// The installed CLI routes MCP calls through its isolated Code Mode host.
	// Enabling this dispatcher exposes no native shell or filesystem tool.
	args = append(args, "-c", "features.code_mode_host=true")
	toolArgs, _ := json.Marshal([]string{"-tool-server", "-tools-config", p.ToolsConfig})
	namesList := p.ToolNames
	if namesList == nil {
		namesList = replaytools.Names
	}
	names, _ := json.Marshal(namesList)
	for _, pair := range []string{
		"mcp_servers.esa_replay.command=" + strconv.Quote(p.ToolCommand),
		"mcp_servers.esa_replay.args=" + string(toolArgs),
		"mcp_servers.esa_replay.enabled_tools=" + string(names),
		"mcp_servers.esa_replay.default_tools_approval_mode=\"approve\"",
		"mcp_servers.esa_replay.tool_timeout_sec=610",
		"mcp_servers.esa_replay.required=true",
	} {
		args = append(args, "-c", pair)
	}
	return append(args, "-")
}

func allowedItem(kind, server, tool string, tools bool) bool {
	switch kind {
	case "", "agent_message", "reasoning", "error":
		return true
	case "mcp_tool_call":
		if !tools || server != "esa_replay" {
			return false
		}
		for _, name := range append(append([]string{}, replaytools.Names...), replaytools.RoutingNames...) {
			if tool == name {
				return true
			}
		}
	}
	return false
}

func (p *Provider) analyzeEngineering(ctx context.Context, in esa.AnalyzeRequest) (result esa.Report, resultErr error) {
	in = engineeringInput(in)
	if p.Slots != nil {
		select {
		case p.Slots <- struct{}{}:
			defer func() { <-p.Slots }()
		case <-ctx.Done():
			return esa.Report{}, ctx.Err()
		}
	}
	if p.Resolve != nil {
		registration, err := p.Resolve(ctx, in)
		if err != nil {
			return esa.Report{}, err
		}
		child := p.child()
		child.Resolve, child.Slots = nil, nil
		child.Packet = registration.Packet
		child.ToolsConfig = registration.ToolsConfig
		child.SetupEnvironment = registration.SetupEnvironment
		return child.analyzeEngineering(ctx, in)
	}
	// Do not queue overlapping provider calls or spend on an unregistered ticket.
	if !p.mu.TryLock() {
		return esa.Report{}, errors.New("Codex provider is busy")
	}
	defer p.mu.Unlock()
	if !in.ReadOnly || in.Ticket.Private || in.Ticket.Closed || in.Budget.MaxCalls != 1 || Fingerprint(in.Ticket) != Fingerprint(p.Packet.Ticket) {
		return esa.Report{}, errors.New("no immutable evidence packet registered for this diagnostic input")
	}
	if in.Contract != nil && (!in.Contract.ReadOnly || in.Contract.ObservationCutoff.Before(p.Packet.Cutoff)) {
		return esa.Report{}, errors.New("registered evidence is outside the task cutoff")
	}
	if !strings.HasPrefix(in.RunID, "run_") || strings.ContainsAny(in.RunID, "/\\.") {
		return esa.Report{}, errors.New("invalid run ID")
	}
	dir := filepath.Join(p.RunsDir, in.RunID)
	if _, err := os.Stat(dir); err == nil {
		b, err := os.ReadFile(filepath.Join(dir, "report.json"))
		if err != nil {
			return esa.Report{}, errors.New("previous attempt exists; inspect it before starting a new run")
		}
		var cached esa.Report
		if err = json.Unmarshal(b, &cached); err != nil {
			return cached, err
		}
		stored, err := os.ReadFile(filepath.Join(dir, "input.json"))
		input, _ := json.Marshal(in)
		if err != nil || !bytes.Equal(stored, input) {
			return esa.Report{}, errors.New("run ID reused with different input")
		}
		if cached.IssueRequest == nil || cached.Issue != nil {
			return esa.Report{}, errors.New("cached engineering report has an obsolete assignment contract; use a new run ID")
		}
		cached.StoredFields = nil
		return cached, nil
	}
	if err := os.MkdirAll(p.RunsDir, 0700); err != nil {
		return esa.Report{}, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return esa.Report{}, err
	}
	input, _ := json.Marshal(in)
	if err := os.WriteFile(filepath.Join(dir, "input.json"), input, 0600); err != nil {
		return esa.Report{}, err
	}
	oldTools := p.ToolsConfig
	if p.ToolsConfig == "" && p.SetupEnvironment != nil {
		setupCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		config, cleanup, err := p.SetupEnvironment(setupCtx, dir)
		cancel()
		if err != nil {
			return esa.Report{}, err
		}
		p.ToolsConfig = config
		defer func() {
			p.ToolsConfig = oldTools
			err := cleanup()
			status := "completed"
			message := ""
			if err != nil {
				status = "failed"
				message = err.Error()
			}
			b, _ := json.Marshal(map[string]string{"status": status, "error": message})
			_ = os.WriteFile(filepath.Join(dir, "cleanup.json"), b, 0600)
			if err != nil {
				slog.Error("Replay environment cleanup failed", "run_id", in.RunID, "error", err)
				esa.AddAlerts(&result, esa.Alert{Code: "cleanup_failed", Message: "Owned environment cleanup failed; inspect the private cleanup audit."})
				if resultErr == nil {
					b, _ = json.MarshalIndent(result, "", "  ")
					_ = os.WriteFile(filepath.Join(dir, "report.json"), b, 0600)
				}
			}
		}()
	}
	var tools replaytools.Config
	var catalog []capabilities.Profile
	var bootstrap string
	var paths []string
	if p.ToolsConfig != "" {
		var err error
		tools, err = replaytools.Load(p.ToolsConfig)
		if err != nil {
			return esa.Report{}, err
		}
		if tools.Commit != p.Packet.Commit || p.ToolCommand == "" {
			return esa.Report{}, errors.New("tool source does not match registered historical commit")
		}
		if in.Contract != nil && p.ProfilesRoot != "" {
			if p.ToolCatalog != nil {
				tools.Tools = p.ToolCatalog
			}
			tools.ProfilesRoot = p.ProfilesRoot
			if len(tools.AllowedProfiles) == 0 {
				tools.AllowedProfiles = []string{"general", "java-debug", "performance", "compatibility", "configuration"}
			}
			if len(tools.AvailableCapabilities) == 0 {
				tools.AvailableCapabilities = []string{"source"}
			}
			planner := capabilities.Planner{
				Root:                  tools.ProfilesRoot,
				AllowedProfiles:       tools.AllowedProfiles,
				AvailableCapabilities: tools.AvailableCapabilities,
			}
			catalog, err = planner.Catalog()
			if err != nil {
				return esa.Report{}, err
			}
			for i := range catalog {
				if tools.ProfileHashes == nil {
					tools.ProfileHashes = map[string]string{}
				}
				tools.ProfileHashes[catalog[i].ID] = catalog[i].ManifestHash
				if catalog[i].ID == "general" {
					bootstrap = catalog[i].Skill
				}
				catalog[i].Skill = ""
				catalog[i].Toolset = ""
			}
		}
		tools.IssuesOnly = false
		tools.IssuesRoot, tools.IssueDirectory, tools.IssueDirectoryHash, tools.IssueContext = "", "", "", ""
		configData, _ := json.Marshal(tools)
		p.ToolsConfig = filepath.Join(dir, "tools-config-engineering.json")
		if err = os.WriteFile(p.ToolsConfig, configData, 0600); err != nil {
			return esa.Report{}, err
		}
		p.ToolNames = append([]string{}, replaytools.Names...)
		for path := range tools.Files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		if len(paths) > 40 {
			paths = paths[:40]
		}
	}
	// Only frozen input and time-visible evidence/candidate conclusions are model-visible.
	packet := struct {
		Contract    *esa.TaskContract              `json:"contract,omitempty"`
		Profiles    []capabilities.Profile         `json:"profile_catalog,omitempty"`
		Cutoff      time.Time                      `json:"cutoff"`
		Ticket      esa.Ticket                     `json:"ticket"`
		Documents   []replaytools.NumberedDocument `json:"documents"`
		Limitations []string                       `json:"limitations"`
		Commit      string                         `json:"source_commit"`
		SourcePaths []string                       `json:"source_paths_preview,omitempty"`
	}{in.Contract, catalog, p.Packet.Cutoff, p.Packet.Ticket, visibleDocuments(p.Packet.Documents), p.Packet.Limitations, p.Packet.Commit, paths}
	payload, _ := json.Marshal(packet)
	instructions := engineeringInstructions(p.ToolsConfig != "", len(catalog) > 0, bootstrap)
	prompt := engineeringPrompt + "\n" + string(payload)
	for name, data := range map[string][]byte{"schema.json": EngineeringSchema(), "instructions.txt": []byte(instructions), "prompt.txt": []byte(prompt)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return esa.Report{}, err
		}
	}
	b, usage, alerts, err := p.invoke(ctx, dir, prompt, in.Ticket.SourceKey, in.Budget)
	// Preserve spend even when output decoding, evidence checks or cleanup fail.
	defer func() {
		result.Usage = usage
		esa.AddAlerts(&result, alerts...)
	}()
	if err != nil {
		return esa.Report{}, err
	}
	var output EngineeringOutput
	if err = decodeAgentOutput(b, &output); err != nil {
		return esa.Report{}, err
	}
	var report esa.Report
	var environmentRequests []esa.EnvironmentRequest
	packetForValidation := p.Packet
	if p.ToolsConfig != "" {
		docs, requests, e := replaytools.ReadAudit(tools.AuditPath)
		if e != nil {
			return report, e
		}
		packetForValidation.Documents = append(packetForValidation.Documents, docs...)
		environmentRequests = requests
	}
	if report, err = output.Report(packetForValidation); err != nil {
		return report, err
	}
	report.EnvironmentRequests = environmentRequests
	if len(catalog) > 0 {
		report.Plan, err = replaytools.ReadPlan(tools.AuditPath)
		if err != nil {
			return report, err
		}
		unlocated := len(report.Findings) == 0 && len(report.Evidence) == 0
		if report.Plan == nil && !unlocated {
			return report, errors.New("Shared investigation diagnosis did not obtain an approved investigation plan")
		}
	}
	report.Usage = usage
	esa.AddAlerts(&report, alerts...)
	report.Limitations = append(report.Limitations, p.Packet.Limitations...)
	report.Limitations = append(report.Limitations, "Token thresholds generate advisory warnings and do not terminate analysis. CLI usage may "+
		"arrive only when the turn completes.")
	b, _ = json.MarshalIndent(report, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "report.json"), b, 0600); err != nil {
		return report, err
	}
	return report, nil
}

func reportedTokens(raw json.RawMessage) (esa.Usage, bool) {
	var u struct {
		Input  *int `json:"input_tokens"`
		Output *int `json:"output_tokens"`
		Cached int  `json:"cached_input_tokens"`
	}
	if json.Unmarshal(raw, &u) != nil || u.Input == nil || u.Output == nil || *u.Input < 0 || *u.Output < 0 || u.Cached < 0 || u.Cached > *u.Input {
		return esa.Usage{}, false
	}
	return esa.Usage{InputTokens: *u.Input, CachedInputTokens: u.Cached, OutputTokens: *u.Output, TokensReported: true}, true
}
