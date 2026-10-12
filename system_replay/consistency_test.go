package systemreplay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEventRefreshesPublicInputWithoutExpandingTicketText(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{}`))
		} else {
			_, _ = w.Write([]byte(`{"state":"completed"}`))
		}
	}))
	defer server.Close()
	client, _ := newClient(Target{URL: server.URL, Submit: Request{Method: "POST", Path: "/events", Body: map[string]any{"key": "{{source_key}}", "subject": "literal: {{ticket.subject}}"}}, Observe: Request{Path: "/results/{{ticket_id}}"}, Extract: Extract{State: []string{"/state"}}, Capture: map[string]string{"case_id": "/case/id"}, TerminalStates: []string{"completed"}, TimeoutSeconds: 1, PollMilliseconds: 1}, "")
	vars := varsFor(Plan{Ticket: map[string]any{"id": "old", "source_key": "OLD", "project_id": "1"}}, t.TempDir())
	vars["case_id"] = "stale"
	result := client.event(context.Background(), Event{Ticket: map[string]any{"id": "new", "source_key": "NEW", "subject": "{{source_key}}"}}, vars)
	if result.Error != "" || body["key"] != "NEW" || body["subject"] != "literal: {{source_key}}" || vars["case_id"] != nil {
		t.Fatal(result, body, vars)
	}
}

func TestInvalidManifestCannotPlanSuccessfully(t *testing.T) {
	root := t.TempDir()
	base := Manifest{Schema: 1, Jobs: []Job{{ID: "a/b", Ticket: map[string]any{"id": "one", "updated_at": "2020-01-01T00:00:00Z"}}, {ID: "a_b", Ticket: map[string]any{"id": "two", "updated_at": "2020-01-01T00:00:00Z"}}}}
	path := filepath.Join(root, "manifest.json")
	_ = write(path, base)
	if _, err := load(path); err == nil {
		t.Fatal("colliding report paths accepted")
	}
	base.Jobs = base.Jobs[:1]
	base.Jobs[0].At = "invalid time"
	_ = write(path, base)
	summary, err := Run(context.Background(), path, filepath.Join(root, "out"), Options{PlanOnly: true})
	if err == nil || summary.Phase != "failed" || summary.Results[0].Error == "" {
		t.Fatal("invalid plan returned success", summary, err)
	}
	raw, _ := json.Marshal(base)
	_ = os.WriteFile(path, append(raw, []byte(" {}")...), 0600)
	if _, err := load(path); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestHistoryReaderPinsRefAndSharesSuccessfulReads(t *testing.T) {
	g, base, fix := history(t)
	reader := &historyReader{g, map[string]string{}}
	head, err := reader.resolve("main")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", g.Repo, "update-ref", "refs/heads/main", fix).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	pinned, err := reader.resolve("main")
	changed, freshErr := g.resolve("main")
	if err != nil || freshErr != nil || pinned != head || changed != fix || base == fix || len(reader.reads) != 1 {
		t.Fatal("mainline was not pinned", pinned, changed, err, freshErr)
	}
}

func TestRuntimeResetFailureRejectsOtherwiseValidObservation(t *testing.T) {
	g, base, _ := history(t)
	root := t.TempDir()
	driver := `while IFS= read -r request; do
case "$request" in
*'"action":"prepare"'*) printf '%s\n' '{"ok":true,"result":{"ready":true}}' ;;
*'"action":"observe"'*) printf '%s\n' '{"ok":true,"result":{"control":true,"bug":true,"healthy":false}}' ;;
*'"action":"reset"'*) printf '%s\n' '{"ok":true,"result":{"reset":false}}' ;;
*'"action":"close"'*) printf '%s\n' '{"ok":true,"result":{"cleanup":"completed"}}'; exit ;;
esac
done`
	runtime, err := runtimeStart(Command{Args: []string{"sh", "-c", driver}}, nil, root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := runtime.close(); err != nil {
			t.Error(err)
		}
	}()
	scenario := &Scenario{Profile: "command", Controls: []Assertion{{"/control", "eq", true}}, Bug: []Assertion{{"/bug", "eq", true}}, Healthy: []Assertion{{"/healthy", "eq", true}}}
	record, err := observeRuntime(context.Background(), runtime, Manifest{Repo: g.Repo}, Plan{Job: Job{Scenario: scenario}}, base, root)
	if err == nil || !strings.Contains(err.Error(), "reset") || !passed(record.Controls) || !passed(record.Bug) {
		t.Fatal("failed reset was ignored", record, err)
	}
}
