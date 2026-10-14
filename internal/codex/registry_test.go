package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"esa/internal/esa"
	"esa/internal/replaytools"
)

func TestRegistrySelectsHistoricalSourceWithoutRuntimeSubstitution(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(date string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test", "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
		return strings.TrimSpace(string(out))
	}
	old := "2019-01-01T00:00:00Z"
	git(old, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "Example.java"), []byte("assignment.size();\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(old, "add", ".")
	git(old, "commit", "-qm", "old")
	git(old, "tag", "release-old")
	commit := git(old, "rev-parse", "HEAD")
	git("2021-01-01T00:00:00Z", "commit", "--allow-empty", "-qm", "future")
	git(old, "tag", "release-new")
	cutoff, _ := time.Parse(time.RFC3339, "2020-01-01T00:00:00Z")
	available, _ := time.Parse(time.RFC3339, old)
	root, _ := filepath.Abs("../../profiles")
	r := Registry{Releases: []ReleaseRegistration{{ProjectID: "1", Version: "1", Ref: "refs/tags/release-old", Repository: repo, Scope: "full", AvailableAt: available}, {ProjectID: "1", Version: "2", Ref: "refs/tags/release-new", Repository: repo, Scope: "full", AvailableAt: available}}, Artifacts: filepath.Join(dir, "cache"), ProfilesRoot: root}
	in := esa.AnalyzeRequest{RunID: "run_registry", ReadOnly: true, Ticket: esa.Ticket{ID: "a", ProjectID: "1", SourceKey: "a", Versions: []string{"1"}, ForwardedAt: cutoff}, Contract: &esa.TaskContract{ReadOnly: true, ObservationCutoff: cutoff}}
	registration, err := r.Resolve(context.Background(), in)
	if err != nil || registration.Packet.Commit != commit {
		t.Fatalf("source: %+v %v", registration, err)
	}
	runDir := filepath.Join(dir, "run")
	if err = os.Mkdir(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := registration.SetupEnvironment(context.Background(), runDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	tools, err := replaytools.Load(path)
	if err != nil || tools.Container != "" || len(tools.AvailableCapabilities) != 1 || tools.AvailableCapabilities[0] != "source" {
		t.Fatalf("unregistered runtime introduced: %+v %v", tools, err)
	}
	if _, err = os.Stat(filepath.Join(tools.SourceDir, ".git")); !os.IsNotExist(err) {
		t.Fatal("future Git history exposed")
	}
	if _, err = (&replaytools.Server{Config: tools}).Call(context.Background(), "read_code", json.RawMessage(`{"path":"Example.java","first_line":1,"line_count":1}`)); err != nil {
		t.Fatal(err)
	}
	in.Ticket.Versions = []string{"2"}
	if _, err = r.Resolve(context.Background(), in); err == nil {
		t.Fatal("future commit accepted")
	}
	for _, versions := range [][]string{{"unknown"}, {"1", "2"}, nil} {
		in.Ticket.Versions = versions
		result, err := r.Resolve(context.Background(), in)
		if err != nil || result.SetupEnvironment != nil || result.Packet.Commit != "" || len(result.Packet.Documents) != 0 || len(result.Packet.Limitations) == 0 {
			t.Fatal("missing environment should reach Agent without invented source/runtime", result, err)
		}
	}
}

func TestContextOnlyRegistrationStillInvokesAgent(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	in := esa.AnalyzeRequest{RunID: "run_missing_release", ReadOnly: true, Ticket: esa.Ticket{ID: "query", ProjectID: "1", SourceKey: "query", Subject: "Failure without version", ForwardedAt: at}, Contract: &esa.TaskContract{ReadOnly: true, ObservationCutoff: at}, Budget: esa.Budget{MaxCalls: 1, TimeoutSeconds: 10}}
	registration, err := (&Registry{}).Resolve(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	answer := EngineeringOutput{IssueRequest: esa.IssueRequest{Title: "More context needed", Reason: "Affected release is unknown"}, Limitations: []string{"Provide the affected version and reproduction steps"}}
	data, _ := json.Marshal(answer)
	fixture := filepath.Join(dir, "answer.json")
	if err = os.WriteFile(fixture, data, 0600); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, "fake-codex")
	script := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\nif [ \"$1\" = '-o' ]; then cp '" + fixture + "' \"$2\"; fi\nshift\ndone\nprintf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":8}}'\n"
	if err = os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	provider := Provider{CLI: cli, RunsDir: filepath.Join(dir, "runs"), Packet: registration.Packet}
	report, err := provider.analyzeEngineering(context.Background(), in)
	if err != nil || len(report.Findings) != 0 || len(report.Evidence) != 0 || len(report.Limitations) == 0 || report.Usage.InputTokens != 12 {
		t.Fatal("context-only packet did not reach Agent", report, err)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "runs", in.RunID, "prompt.txt"))
	if err != nil || !strings.Contains(string(prompt), "No product source or runtime") {
		t.Fatal("Agent did not receive environment limitation", err)
	}
}
