package systemreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPBoundaryUsesPublicRequestsAndObservedResults(t *testing.T) {
	var mu sync.Mutex
	events := []map[string]any{}
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer independent-test-token" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "POST" {
			var event map[string]any
			_ = json.NewDecoder(r.Body).Decode(&event)
			events = append(events, event)
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": "external-run"})
			return
		}
		gets++
		state := "queued"
		if gets >= 2 {
			state = "completed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"external_state": state, "external_class": "actual_target_class", "external_route": "actual_target_route", "usage": map[string]any{"calls": 0, "tokens_reported": true, "input_tokens": 0, "output_tokens": 0}})
	}))
	defer server.Close()
	target := Target{URL: server.URL, TokenEnv: "REPLAY_TEST_TOKEN", Submit: Request{Method: "POST", Path: "/events", Body: map[string]any{"id": "{{event_id}}", "input": "{{ticket}}"}}, Observe: Request{Path: "/results/{{ticket_id}}"}, Extract: Extract{State: []string{"/external_state"}, Category: []string{"/external_class"}, Route: []string{"/external_route"}, Usage: []string{"/usage"}}, TerminalStates: []string{"completed"}, PollMilliseconds: 1, TimeoutSeconds: 5}
	job := Job{ID: "ticket-1", Version: "1.0", At: "2020-01-01T00:00:00Z", Ticket: map[string]any{"id": "ticket-1", "source_key": "ticket-1", "project_id": "project", "subject": "raw symptom", "updated_at": "2020-01-01T00:00:00Z"}, Expect: []Assertion{{"/category", "eq", "actual_target_class"}, {"/route", "eq", "actual_target_route"}}}
	root := t.TempDir()
	manifest := filepath.Join(root, "test.json")
	if e := write(manifest, Manifest{Schema: 1, Target: target, Jobs: []Job{job}}); e != nil {
		t.Fatal(e)
	}
	t.Setenv("REPLAY_TEST_TOKEN", "independent-test-token")
	result, e := Run(context.Background(), manifest, filepath.Join(root, "output"), Options{Attach: true})
	if e != nil {
		t.Fatal(e)
	}
	if len(events) != 1 || gets < 2 || result.Results[0].Events[0].Observation.Category != "actual_target_class" {
		t.Fatal("tester did not observe the target", result)
	}
	input := events[0]["input"].(map[string]any)
	if input["subject"] != "raw symptom" || input["classification"] != nil {
		t.Fatal("tester classified or altered input", input)
	}
	if strings.Contains(fmt.Sprint(events), "actual_target_class") {
		t.Fatal("assertion labels leaked into system input")
	}
	b, _ := os.ReadFile(filepath.Join(root, "output/jobs/ticket-1/system-output.json"))
	if sha(b) != result.Results[0].PredictionHash {
		t.Fatal("public output not frozen")
	}
	if result.Cleanup != "runtime_cleaned_target_external" {
		t.Fatal(result.Cleanup)
	}
}
func TestManagedModeRefusesExistingTargetWithoutMutation(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	root := t.TempDir()
	m := Manifest{Schema: 1, Target: Target{URL: server.URL, Submit: Request{Path: "/events"}, Observe: Request{Path: "/results"}, TerminalStates: []string{"completed"}}, Jobs: []Job{{ID: "one", Ticket: map[string]any{"id": "one", "updated_at": "2020-01-01T00:00:00Z"}}}}
	path := filepath.Join(root, "manifest.json")
	_ = write(path, m)
	_, e := Run(context.Background(), path, filepath.Join(root, "out"), Options{})
	if e == nil || writes != 0 {
		t.Fatal("existing target was written", e, writes)
	}
}
func TestHistoricalVersionOrderCannotCreateFutureKnowledge(t *testing.T) {
	root := t.TempDir()
	m := Manifest{Schema: 1, Target: Target{URL: "http://127.0.0.1:1", Submit: Request{Path: "/events"}, Observe: Request{Path: "/results"}, TerminalStates: []string{"completed"}}, Jobs: []Job{{ID: "old-version-late-ticket", Version: "1.0", Ticket: map[string]any{"id": "1", "updated_at": "2021-01-01T00:00:00Z"}}, {ID: "new-version-early-ticket", Version: "2.0", Ticket: map[string]any{"id": "2", "updated_at": "2020-01-01T00:00:00Z"}}}}
	path := filepath.Join(root, "manifest.json")
	_ = write(path, m)
	_, e := Run(context.Background(), path, filepath.Join(root, "out"), Options{Pipeline: true})
	if e == nil || !strings.Contains(e.Error(), "future events") {
		t.Fatal(e)
	}
}
func TestBehaviorGateRequiresSameScenarioFailureAndControl(t *testing.T) {
	check := func(v bool) []Check { return []Check{{Passed: v}} }
	base := RuntimeRecord{ScenarioHash: "same", Controls: check(true), Bug: check(true), Healthy: check(false)}
	before := base
	after := RuntimeRecord{ScenarioHash: "same", Controls: check(true), Bug: check(false), Healthy: check(true)}
	if state := repairState(base, before, after); state != "behavior_verified" {
		t.Fatal(state)
	}
	before.Bug = check(false)
	if repairState(base, before, after) != "parent_not_reproduced" {
		t.Fatal("unreproduced parent accepted")
	}
	before = base
	after.Controls = check(false)
	if repairState(base, before, after) != "environment_failure" {
		t.Fatal("broken control accepted")
	}
	after.Controls = check(true)
	after.ScenarioHash = "different"
	if repairState(base, before, after) != "scenario_changed" {
		t.Fatal("changed scenario accepted")
	}
}
func TestStartupFailureCleansOwnedProcess(t *testing.T) {
	root := t.TempDir()
	p, e := start(Command{Args: []string{"sh", "-c", "exit 9"}}, nil, root, filepath.Join(root, "log"))
	if e != nil {
		t.Fatal(e)
	}
	client, _ := newClient(Target{URL: "http://127.0.0.1:1", HealthPath: "/healthz"}, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if waitHealth(ctx, client, p) == nil {
		t.Fatal("failed process became healthy")
	}
	began := time.Now()
	if e = p.close(); e != nil {
		t.Fatal(e)
	}
	if time.Since(began) > time.Second {
		t.Fatal("startup failure consumed cleanup completion")
	}
}
func TestJiraSnapshotWithholdsFutureResolutionAndEditedBodies(t *testing.T) {
	raw := []byte(`{"metadata":{"history_complete":true,"history_reported_total":1},"issue":{"key":"BUG-1","fields":{"created":"2020-01-01T00:00:00.000+0000","summary":"future summary","description":"symptom","issuetype":{"name":"Bug"},"status":{"name":"Resolved"},"versions":[{"name":"1.0"}],"resolution":{"name":"Fixed"},"fixVersions":[{"name":"1.1"}],"comment":{"comments":[{"id":"1","created":"2020-01-01T00:00:00Z","updated":"2020-02-01T00:00:00Z","body":"future repair"}]}}},"changelog":[{"created":"2020-02-01T00:00:00Z","items":[{"field":"summary","fromString":"old summary","toString":"future summary"},{"field":"status","fromString":"Open","toString":"Resolved"}]}]}`)
	ticket, _, _, e := jira(raw, Job{Project: "A"})
	if e != nil {
		t.Fatal(e)
	}
	if ticket["subject"] != "old summary" || ticket["closed"] != false || len(ticket["comments"].([]any)) != 0 || len(ticket["resolution"].(map[string]any)) != 0 {
		t.Fatal(ticket)
	}
	if strings.Contains(fmt.Sprint(ticket), "future repair") {
		t.Fatal("future body leaked")
	}
}

func TestExternalEvaluatorCannotOverrideReplayGateAndMustCiteEvidence(t *testing.T) {
	g, base, fix := history(t)
	calls := 0
	invent := false
	patch, _ := g.run("diff", "--unified=0", fix+"^", fix)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		quote := strings.Split(patch, "\n")[0]
		if invent {
			quote = "invented patch text"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"findings": []any{map[string]any{"index": 0, "localization": "correct", "problem": "correct", "solution": "unknown", "explanation": "Fixture evaluator protocol."}}, "evidence": []any{map[string]any{"source": "patch", "reference": fix + ":patch:1", "quote": quote}}})
	}))
	defer server.Close()
	m := Manifest{Repo: g.Repo, Evaluator: &Evaluator{URL: server.URL, Request: Request{Method: "POST", Path: "/evaluate", Body: "{{evaluation_packet}}"}}}
	result := Result{Plan: Plan{Commit: base, Ticket: map[string]any{"source_key": "BUG-1"}}, Events: []EventResult{{Observation: Observation{Findings: []Finding{{Commit: base, File: "old.java", StartLine: 2, EndLine: 2, Problem: "Fixture cause", Solution: "Fixture correction"}}}}}}
	// Later lifecycle observations must not replace the original diagnosis.
	result.Events = append(result.Events, EventResult{Observation: Observation{Findings: []Finding{{Commit: fix, File: "new.java", StartLine: 3, EndLine: 3, Problem: "Later result", Solution: "Later suggestion"}}}})
	repair := Repair{Candidate: Candidate{Commit: fix, Patch: patch}, State: "not_fixed"}
	if response, e := evaluateRepair(context.Background(), m, result, repair); e != nil || response != nil || calls != 0 {
		t.Fatal("unverified behavior sent for accuracy evaluation", response, e, calls)
	}
	repair.State = "behavior_verified"
	if response, e := evaluateRepair(context.Background(), m, result, repair); e != nil || response == nil || calls != 1 {
		t.Fatal(response, e, calls)
	}
	invent = true
	if _, e := evaluateRepair(context.Background(), m, result, repair); e == nil {
		t.Fatal("fabricated semantic evidence accepted")
	}
}
func TestAdvisoryUsageAggregatesWithoutDoubleCountingPublicRun(t *testing.T) {
	result := Result{ID: "ticket"}
	m := Manifest{WarnInputTokens: 10, WarnOutputTokens: 10, WarnTotalTokens: 15}
	usage := map[string]any{"tokens_reported": true, "input_tokens": float64(6), "output_tokens": float64(4)}
	tokenWarnings(&result, usage, m, "run-a")
	tokenWarnings(&result, usage, m, "run-a")
	if len(result.Alerts) != 0 || result.MeasuredUsage["input_tokens"] != 6 {
		t.Fatal(result)
	}
	tokenWarnings(&result, usage, m, "run-b")
	if len(result.Alerts) != 2 || result.MeasuredUsage["input_tokens"] != 12 {
		t.Fatal(result)
	}
	tokenWarnings(&result, nil, m, "run-c")
	if len(result.Alerts) != 3 {
		t.Fatal("unknown usage silently treated as zero")
	}
}
