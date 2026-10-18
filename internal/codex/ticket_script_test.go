package codex

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This exercises the Python entry point, a separate real ESA service/worker,
// the provider HTTP API and the actual role-specific adapter/tool handlers.
// Only the model response is a subprocess fixture; no model credits are used.
func TestTicketScriptRunsPipelineAndDownloadsOriginalEvents(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "esa-host")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./cmd/esa")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ESA: %v\n%s", err, output)
	}
	for _, tc := range []struct {
		decision string
		state    string
		stages   []string
		fails    bool
	}{
		{"analyze", "awaiting_review", []string{"route", "engineer", "assign"}, false},
		{"return_to_support", "returned_to_support", []string{"route"}, false},
		{"assignment_no_request", "failed", []string{"route", "engineer", "assign"}, true},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			p, _, _ := routingFixture(t, tc.decision)
			token := strings.Repeat("t", 32)
			t.Setenv("ESA_ANALYZER_TOKEN", token)
			t.Setenv("ESA_DATABASE_URL", os.Getenv("ESA_TEST_DATABASE_URL"))
			server := httptest.NewServer(p.Handler(token))
			defer server.Close()
			folder := t.TempDir()
			ticket := filepath.Join(folder, "ticket.json")
			if err := os.WriteFile(ticket, []byte(`{"id":"script-ticket","project_id":"1","subject":"Example failure","versions":["1"]}`), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(folder, "output")
			command := exec.Command(python, filepath.Join(root, "scripts/analyze_ticket.py"), ticket,
				"--routing-provider", server.URL, "--esa-bin", binary, "--issues-directory", p.IssuesRoot,
				"--output", output, "--wait-seconds", "30")
			command.Dir = root
			stdout, err := command.CombinedOutput()
			if (err != nil) != tc.fails {
				t.Fatalf("script failed unexpectedly: %v\n%s", err, stdout)
			}
			raw, err := os.ReadFile(filepath.Join(output, "result.json"))
			if err != nil {
				t.Fatalf("missing result: %v\n%s", err, stdout)
			}
			var result struct {
				Ticket struct {
					LatestRun struct {
						ID    string `json:"id"`
						State string `json:"state"`
					} `json:"latest_run"`
				} `json:"ticket"`
			}
			if err := json.Unmarshal(raw, &result); err != nil || result.Ticket.LatestRun.State != tc.state {
				t.Fatalf("unexpected final state: %v\n%s\n%s", err, raw, stdout)
			}
			id := result.Ticket.LatestRun.ID
			files := map[string]string{"route": "routing", "engineer": "engineering", "assign": "assignment"}
			for _, stage := range tc.stages {
				original := filepath.Join(p.RunsDir, id, files[stage], "events.jsonl")
				if stage == "engineer" {
					original = filepath.Join(p.RunsDir, id, "engineering", id, "events.jsonl")
				}
				expected, err := os.ReadFile(original)
				if err != nil {
					t.Fatal(err)
				}
				actual, err := os.ReadFile(filepath.Join(output, "logs", id, files[stage]+".events.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				var a, b bytes.Buffer
				if json.Compact(&a, expected) != nil || json.Compact(&b, actual) != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
					t.Fatal("download changed or duplicated the provider event", stage, string(actual))
				}
			}
			if tc.decision == "return_to_support" {
				if _, err := os.Stat(filepath.Join(output, "logs", id, "engineering.events.jsonl")); !os.IsNotExist(err) {
					t.Fatal("script fabricated engineering events for a support request")
				}
			}
		})
	}
}
