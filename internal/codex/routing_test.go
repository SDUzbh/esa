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
	// quota_<stage> refuses that stage once with the CLI's usage-limit events,
	// then behaves like analyze.
	if refused, ok := strings.CutPrefix(decision, "quota_"); ok {
		stage := map[string]string{"routing": "route", "assignment": "assign"}[filepath.Base(dir)]
		if stage == "" {
			stage = "engineer"
		}
		if marker := os.Getenv("QUOTA_MARKER"); refused == stage {
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				_ = os.WriteFile(marker, nil, 0600)
				message := "You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro) or try again at 11:19 PM."
				fmt.Printf("{\"type\":\"error\",\"message\":%q}\n{\"type\":\"turn.failed\",\"error\":{\"message\":%q}}\n", message, message)
				os.Exit(1)
			}
		}
		decision = "analyze"
	}
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
		if strings.HasPrefix(decision, "join") || decision == "reuse_pending" {
			kind = "investigating"
		}
		args, _ := json.Marshal(map[string]any{"query": "", "kind": kind, "limit": 20})
		var result map[string]any
		if decision != "return_to_support" {
			result, err = server.Call(context.Background(), "issue_search", args)
			if err != nil {
				panic(err)
			}
		}
		packet, _ := json.Marshal(result)
		var found struct {
			Entries []esa.IssueIndexEntry `json:"entries"`
		}
		_ = json.Unmarshal(packet, &found)
		routing := map[string]string{"action": "analyze", "record_path": "", "record_hash": "", "reason": "No reusable solution covers this ticket"}
		if strings.HasPrefix(decision, "reuse") || strings.HasPrefix(decision, "join") || decision == "no_read" || decision == "invent" {
			entry := found.Entries[0]
			if decision != "no_read" && decision != "join_unread" {
				args, _ = json.Marshal(map[string]string{"path": entry.Path, "sha256": entry.SHA256})
				if _, err = server.Call(context.Background(), "issue_read", args); err != nil {
					panic(err)
				}
			}
			routing = map[string]string{"action": "reuse", "record_path": entry.Path, "record_hash": entry.SHA256, "reason": "The record covers the same failing behavior, version and environment"}
			if strings.HasPrefix(decision, "join") {
				routing["action"] = "join"
			}
			if decision == "invent" {
				routing["record_path"] = "engineer_resolved/invented/issue.json"
			}
		} else if decision == "return_to_support" {
			routing["action"] = "return_to_support"
			routing["reason"] = "No software defect is reported. Ask Billing for the invoice download process."
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
	} else if filepath.Base(dir) == "assignment" {
		raw, err := os.ReadFile(filepath.Join(dir, "tools-config.json"))
		if err != nil {
			panic(err)
		}
		var config replaytools.Config
		if err = json.Unmarshal(raw, &config); err != nil {
			panic(err)
		}
		server := replaytools.Server{Config: config}
		if decision != "assignment_no_request" {
			context, err := server.Call(context.Background(), "issue_context", json.RawMessage(`{}`))
			if err != nil || context["routing_context"] == nil {
				panic("Routing Agent could not read engineering request")
			}
		}
		grouping := map[string]string{"action": "create", "record_path": "", "record_hash": "", "title": "Routing approved canonical title", "reason": "The inspected diagnosis describes a separate cause"}
		if decision == "assign_existing" || decision == "assignment_unread" {
			result, err := server.Call(context.Background(), "issue_search", json.RawMessage(`{"query":"","limit":20}`))
			if err != nil {
				panic(err)
			}
			packet, _ := json.Marshal(result)
			var found struct {
				Entries []esa.IssueIndexEntry `json:"entries"`
			}
			if err = json.Unmarshal(packet, &found); err != nil {
				panic(err)
			}
			entry := found.Entries[0]
			if decision != "assignment_unread" {
				args, _ := json.Marshal(map[string]string{"path": entry.Path, "sha256": entry.SHA256})
				if _, err = server.Call(context.Background(), "issue_read", args); err != nil {
					panic(err)
				}
			}
			grouping["action"], grouping["record_path"], grouping["record_hash"] = "assign", entry.Path, entry.SHA256
		} else if decision == "assignment_info" {
			grouping["action"], grouping["title"] = "request_information", ""
		} else if decision == "assignment_override" {
			grouping["action"] = "assign"
		}
		if _, err = server.Call(context.Background(), "read_code", json.RawMessage(`{"path":"Example.java"}`)); err == nil {
			panic("Routing Agent acquired product source access")
		}
		raw, _ = json.Marshal(grouping)
		if err = os.WriteFile(output, raw, 0600); err != nil {
			panic(err)
		}
	} else {
		answer := EngineeringOutput{IssueRequest: esa.IssueRequest{Title: "A newly diagnosed defect", Reason: "Observed in frozen source"}, Findings: []EngineeringFinding{selectedFinding("old", "Example.java", 1, "bug();", "An empty value is dereferenced", "Guard the empty value")}}
		raw, _ := json.Marshal(answer)
		if err := os.WriteFile(output, raw, 0600); err != nil {
			panic(err)
		}
	}
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":12,"cached_input_tokens":8,"output_tokens":8}}`)
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
		if in.IssueDirectory != "" || in.IssueDirectoryHash != "" || in.Contract != nil && in.Contract.IssueID != "" {
			t.Fatal("engineering source registration received Issue authority or catalogue context")
		}
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
				if len(report.AccessedIssueIDs) != 1 || report.AccessedIssueIDs[0] != "issue_"+map[string]string{"agent": "agent_analyzed", "engineer": "engineer_resolved"}[kind] {
					t.Fatal("verified read was not reported as an access", report.AccessedIssueIDs)
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
	if err != nil || *sourceCalls != 1 || report.Usage.Calls != 3 || report.Usage.InputTokens != 36 || len(report.Findings) != 1 || report.Routing.Action != "analyze" {
		t.Fatal(report, sourceCalls, err)
	}
	if len(report.AccessedIssueIDs) != 0 {
		t.Fatal("search-only routing refreshed candidate access order")
	}
	contextPath := filepath.Join(p.RunsDir, in.RunID, "routing", "assignment-request.json")
	raw, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE_CATALOGUE_SOLUTION") || !strings.Contains(string(raw), "issue_search") {
		t.Fatal("handoff duplicated solution bodies or lost the search trail", string(raw))
	}
	server := replaytools.Server{Config: replaytools.Config{IssuesOnly: true, AuditPath: filepath.Join(t.TempDir(), "tools.jsonl"), IssuesRoot: p.IssuesRoot, IssueDirectory: in.IssueDirectory, IssueDirectoryHash: in.IssueDirectoryHash, IssueContext: contextPath}}
	context, err := server.Call(context.Background(), "issue_context", json.RawMessage(`{}`))
	if err != nil || context["routing_context"] == nil {
		t.Fatal("Routing Agent cannot read the engineering handoff", err)
	}
	engineeringPrompt, err := os.ReadFile(filepath.Join(p.RunsDir, in.RunID, "engineering", in.RunID, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{"issue_directory", "issue_context", "assignment-request.json", "PRIVATE_CATALOGUE_SOLUTION"} {
		if strings.Contains(string(engineeringPrompt), denied) {
			t.Fatal("engineering received catalogue context", string(engineeringPrompt))
		}
	}
	engineeringReport, err := os.ReadFile(filepath.Join(p.RunsDir, in.RunID, "engineering", in.RunID, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request esa.Report
	if err = json.Unmarshal(engineeringReport, &request); err != nil || request.Issue != nil || request.IssueRequest == nil || report.AssignmentRouting == nil || report.Issue.Title != "Routing approved canonical title" {
		t.Fatal("engineering controlled the final assignment", string(engineeringReport), report, err)
	}
	wrong := strings.Replace(string(raw), in.IssueDirectoryHash, "wrong-directory", 1)
	wrongPath := filepath.Join(t.TempDir(), "wrong-context.json")
	if err = os.WriteFile(wrongPath, []byte(wrong), 0600); err != nil {
		t.Fatal(err)
	}
	server.Config.IssueContext = wrongPath
	if _, err = server.Call(t.Context(), "issue_context", json.RawMessage(`{}`)); err == nil {
		t.Fatal("handoff from another directory was accepted")
	}
}

func TestAssignmentRoutingControlsGroupingAndFailureSpend(t *testing.T) {
	for _, decision := range []string{"assign_existing", "assignment_info", "assignment_override", "assignment_unread", "assignment_no_request"} {
		t.Run(decision, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, decision)
			if decision == "assignment_override" {
				in.Contract.IssueID = "reviewed-human-issue"
			}
			report, err := p.Analyze(context.Background(), in)
			if report.Usage.Calls != 3 || report.Usage.InputTokens != 36 || *sourceCalls != 1 || len(report.Findings) != 1 {
				t.Fatal("grouping lost stage costs or original findings", report, err)
			}
			switch decision {
			case "assignment_unread", "assignment_no_request":
				if err == nil || report.Issue != nil {
					t.Fatal("unread grouping was accepted", report, err)
				}
			case "assignment_info":
				if err != nil || report.Issue != nil || report.AssignmentRouting.Action != "request_information" {
					t.Fatal("grouping clarification assigned an Issue", report, err)
				}
			case "assignment_override":
				if err != nil || report.Issue == nil || report.Issue.ExistingID != in.Contract.IssueID {
					t.Fatal("reviewed override lost", report, err)
				}
			default:
				if err != nil || report.Issue == nil || report.Issue.ExistingID == "" || report.AssignmentRouting.Action != "assign" || len(report.AccessedIssueIDs) != 1 {
					t.Fatal("Routing Agent did not assign the existing Issue", report, err)
				}
			}
		})
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
