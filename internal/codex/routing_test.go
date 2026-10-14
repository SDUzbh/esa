package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"esa/internal/esa"
	"esa/internal/replaytools"
)

// A subprocess fixture runs the actual directory tool handlers and returns the
// strict Agent schemas. It performs no model call or product/runtime work.
func TestRoutingCLIHelper(t *testing.T) {
	if os.Getenv("ROUTING_TEST_HELPER") != "1" {
		return
	}
	arg := func(name string) string {
		for i, a := range os.Args {
			if a == name && i+1 < len(os.Args) {
				return os.Args[i+1]
			}
		}
		return ""
	}
	dir, output, decision := arg("-C"), arg("-o"), arg("--model")
	if filepath.Base(dir) == "routing" {
		raw, err := os.ReadFile(filepath.Join(dir, "tools-config.json"))
		if err != nil {
			panic(err)
		}
		var config replaytools.Config
		if err = json.Unmarshal(raw, &config); err != nil {
			panic(err)
		}
		server := replaytools.Server{Config: config}
		kind := ""
		if decision == "reuse_engineer" {
			kind = "engineer_resolved"
		}
		if decision == "reuse_agent" {
			kind = "agent_analyzed"
		}
		args, _ := json.Marshal(map[string]any{"query": "", "kind": kind, "limit": 20})
		result, err := server.Call(context.Background(), "issue_search", args)
		if err != nil {
			panic(err)
		}
		packet, _ := json.Marshal(result)
		var found struct {
			Entries []esa.IssueIndexEntry `json:"entries"`
		}
		_ = json.Unmarshal(packet, &found)
		routing := map[string]string{"action": "analyze", "record_path": "", "record_hash": "", "reason": "No reusable solution covers this ticket"}
		if strings.HasPrefix(decision, "reuse") || decision == "no_read" || decision == "invent" {
			entry := found.Entries[0]
			if decision != "no_read" {
				args, _ = json.Marshal(map[string]string{"path": entry.Path, "sha256": entry.SHA256})
				if _, err = server.Call(context.Background(), "issue_read", args); err != nil {
					panic(err)
				}
			}
			routing = map[string]string{"action": "reuse", "record_path": entry.Path, "record_hash": entry.SHA256, "reason": "The record covers the same failing behavior, version and environment"}
			if decision == "invent" {
				routing["record_path"] = "engineer_resolved/invented/issue.json"
			}
		} else if decision == "request_information" {
			routing["action"] = "request_information"
			routing["reason"] = "Provide the affected release"
		}
		if _, err = server.Call(context.Background(), "run", json.RawMessage(`{"script":"true"}`)); err == nil {
			panic("Routing Agent acquired runtime access")
		}
		raw, _ = json.Marshal(routing)
		if err = os.WriteFile(output, raw, 0600); err != nil {
			panic(err)
		}
	} else {
		answer := EngineeringOutput{Issue: esa.IssueAssignment{Title: "A newly diagnosed defect", Reason: "Observed in frozen source"}, Findings: []esa.Finding{{Commit: "old", File: "Example.java", StartLine: 1, EndLine: 1, Problem: "An empty value is dereferenced", Solution: "Guard the empty value", Evidence: []esa.Evidence{{Source: "git", Reference: "old:Example.java:1", Quote: "bug();"}}}}}
		raw, _ := json.Marshal(answer)
		if err := os.WriteFile(output, raw, 0600); err != nil {
			panic(err)
		}
	}
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":8}}`)
	os.Exit(0)
}

func routingFixture(t *testing.T, decision string) (*Provider, esa.AnalyzeRequest, *int) {
	t.Helper()
	t.Setenv("ROUTING_TEST_HELPER", "1")
	root := t.TempDir()
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []esa.IssueRecord{}
	for _, kind := range []string{"agent_analyzed", "engineer_resolved"} {
		records = append(records, esa.IssueRecord{ID: "issue_" + kind, IssueID: "issue_" + kind, ProjectID: "1", ScopeHash: "scope", Kind: kind, Title: "Existing assignment defect", SourceTicketID: "source", SourceKey: "BUG-0", RunID: "run_original", Summary: "PRIVATE_CATALOGUE_SOLUTION_" + kind, Recommendations: []string{"Guard the known assignment"}, VerificationSteps: []string{"Use the stored regression"}, AvailableAt: at})
	}
	relative, digest, err := esa.ExportIssueDirectory(root, "1", at, records)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "fake-codex")
	if err = os.WriteFile(cli, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(executable, "'", "'\\''")+"' -test.run=TestRoutingCLIHelper -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ticket := esa.Ticket{ID: "new", ProjectID: "1", SourceKey: "BUG-1", Subject: "Failure", ForwardedAt: at, Versions: []string{"1"}}
	in := esa.AnalyzeRequest{RunID: "run_routing", Ticket: ticket, IssueDirectory: relative, IssueDirectoryHash: digest, ReadOnly: true, Contract: &esa.TaskContract{ReadOnly: true, ObservationCutoff: at}, Budget: esa.Budget{MaxCalls: 1, MaxInputTokens: 10000, MaxOutputTokens: 1000, TimeoutSeconds: 20}}
	sourceCalls := new(int)
	p := &Provider{CLI: cli, ToolCommand: "unused-fixture", Model: decision, RunsDir: t.TempDir(), IssuesRoot: root, Resolve: func(_ context.Context, in esa.AnalyzeRequest) (Registration, error) {
		*sourceCalls++
		return Registration{Packet: Packet{Ticket: in.Ticket, Commit: "old", Cutoff: at, Documents: []Document{{Source: "git", Prefix: "old:Example.java:", First: 1, Lines: []string{"bug();"}}}}}, nil
	}}
	return p, in, sourceCalls
}
func TestRoutingReusesBothIssueKindsWithoutEngineering(t *testing.T) {
	for _, kind := range []string{"agent", "engineer"} {
		t.Run(kind, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, "reuse_"+kind)
			for i := 0; i < 2; i++ {
				report, err := p.Analyze(context.Background(), in)
				if err != nil || report.Routing == nil || report.Routing.Action != "reuse" || report.Usage.Calls != 1 || report.Usage.InputTokens != 12 || len(report.Findings) != 0 || len(report.Recommendations) != 1 {
					t.Fatal("reuse failed", report, err)
				}
			}
			if *sourceCalls != 0 {
				t.Fatal("reuse resolved engineering source/runtime")
			}
			prompt, err := os.ReadFile(filepath.Join(p.RunsDir, in.RunID, "routing", "prompt.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(prompt), "PRIVATE_CATALOGUE_SOLUTION") || strings.Contains(string(prompt), "Guard the known assignment") || strings.Contains(string(prompt), "known_issues") || strings.Contains(string(prompt), "eligible_cases") {
				t.Fatal("catalogue was inlined", string(prompt))
			}
			if !strings.Contains(string(prompt), filepath.Join(p.IssuesRoot, filepath.FromSlash(in.IssueDirectory))) {
				t.Fatal("directory path missing")
			}
			if _, err = os.Stat(filepath.Join(p.RunsDir, in.RunID, "engineering")); !os.IsNotExist(err) {
				t.Fatal("reuse initialized engineering stage")
			}
		})
	}
}
func TestRoutingCallsEngineeringOnlyForNewProblem(t *testing.T) {
	p, in, sourceCalls := routingFixture(t, "analyze")
	report, err := p.Analyze(context.Background(), in)
	if err != nil || *sourceCalls != 1 || report.Usage.Calls != 2 || report.Usage.InputTokens != 24 || len(report.Findings) != 1 || report.Routing.Action != "analyze" {
		t.Fatal(report, sourceCalls, err)
	}
}
func TestRoutingClarifiesWithoutEngineering(t *testing.T) {
	p, in, sourceCalls := routingFixture(t, "request_information")
	report, err := p.Analyze(context.Background(), in)
	if err != nil || *sourceCalls != 0 || report.Usage.Calls != 1 || len(report.Evidence) != 0 || report.Routing.Action != "request_information" {
		t.Fatal(report, sourceCalls, err)
	}
}
func TestRoutingRejectsUnreadOrInventedSolutions(t *testing.T) {
	for _, decision := range []string{"no_read", "invent"} {
		t.Run(decision, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, decision)
			if _, err := p.Analyze(context.Background(), in); err == nil || *sourceCalls != 0 {
				t.Fatal("unread/invented solution accepted", err)
			}
		})
	}
}
