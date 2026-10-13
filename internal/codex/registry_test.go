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
	git(old, "tag", "v1")
	commit := git(old, "rev-parse", "HEAD")
	git("2021-01-01T00:00:00Z", "commit", "--allow-empty", "-qm", "future")
	git(old, "tag", "v2")
	cutoff, _ := time.Parse(time.RFC3339, "2020-01-01T00:00:00Z")
	available, _ := time.Parse(time.RFC3339, old)
	root, _ := filepath.Abs("../../profiles")
	r := Registry{Releases: []ReleaseRegistration{{ProjectID: "1", Version: "1", Ref: "refs/tags/v1", Repository: repo, Scope: "full", AvailableAt: available}, {ProjectID: "1", Version: "2", Ref: "refs/tags/v2", Repository: repo, Scope: "full", AvailableAt: available}}, Artifacts: filepath.Join(dir, "cache"), ProfilesRoot: root}
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
	in.Ticket.Versions = []string{"unknown"}
	if _, err = r.Resolve(context.Background(), in); err == nil {
		t.Fatal("unregistered release accepted")
	}
	in.Ticket.Versions = []string{"1", "2"}
	if _, err = r.Resolve(context.Background(), in); err == nil {
		t.Fatal("ambiguous release accepted")
	}
}
