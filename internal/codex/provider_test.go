package codex

import (
	"context"
	"encoding/json"
	"esa/internal/esa"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvidenceRejectsInventedQuoteAndFutureSource(t *testing.T) {
	p := Packet{Documents: []Document{{Source: "git", Prefix: "historical:file.java:", First: 10, Lines: []string{"complete(response);"}}}}
	r := esa.Report{Summary: "Cause", RootCauseCandidates: []string{"Candidate"}, Recommendations: []string{"Repair"}, VerificationSteps: []string{"Verify"}, Evidence: []esa.Evidence{{Source: "git", Reference: "historical:file.java:10", Quote: "complete(response);"}}}
	if err := ValidateEvidence(r, p); err != nil {
		t.Fatal(err)
	}
	r.Evidence[0].Quote = "invented code"
	if ValidateEvidence(r, p) == nil {
		t.Fatal("invented quote accepted")
	}
	r.Evidence[0].Quote = "complete(response);"
	r.Evidence[0].Reference = "future:file.java:10"
	if ValidateEvidence(r, p) == nil {
		t.Fatal("future source accepted")
	}
}

func TestProviderCachesOneInvocationAndRejectsChangedInput(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "invocations")
	fixture := filepath.Join(dir, "fixture.json")
	r := EngineeringOutput{Issue: esa.IssueAssignment{Title: "Disconnect", Reason: "New defect"}, Findings: []esa.Finding{{Commit: "old", File: "file.java", StartLine: 10, EndLine: 10, Problem: "Disconnect", Solution: "Fail and retry", Evidence: []esa.Evidence{{Source: "git", Reference: "old:file.java:10", Quote: "complete(response);"}}}}}
	b, _ := json.Marshal(r)
	os.WriteFile(fixture, b, 0600)
	script := filepath.Join(dir, "fake-codex")
	code := "#!/bin/sh\nprintf x >> '" + counter + "'\nwhile [ \"$#\" -gt 0 ]; do\nif [ \"$1\" = '-o' ]; then cp '" + fixture + "' \"$2\"; fi\nshift\ndone\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"error\",\"message\":\"Under-development features enabled\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":24}}'\n"
	os.WriteFile(script, []byte(code), 0700)
	ticket := esa.Ticket{SourceKey: "BUG-1", ProjectID: "1", Subject: "Crash"}
	p := Provider{CLI: script, RunsDir: filepath.Join(dir, "runs"), Packet: Packet{Commit: "old", Cutoff: time.Now(), Ticket: ticket, Documents: []Document{{Source: "git", Prefix: "old:file.java:", First: 10, Lines: []string{"complete(response);"}}}}}
	in := esa.AnalyzeRequest{RunID: "run_abc123", Ticket: ticket, ReadOnly: true, Budget: esa.Budget{MaxCalls: 1, MaxInputTokens: 10000, MaxOutputTokens: 1000, TimeoutSeconds: 10}}
	for i := 0; i < 2; i++ {
		report, err := p.analyzeEngineering(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if report.Usage.InputTokens != 12 || !report.Usage.TokensReported {
			t.Fatal("actual usage unavailable")
		}
	}
	count, _ := os.ReadFile(counter)
	if string(count) != "x" {
		t.Fatal("duplicate run spent a second invocation")
	}
	in.Ticket.Description = "Changed"
	if _, err := p.analyzeEngineering(context.Background(), in); err == nil {
		t.Fatal("changed diagnostic input accepted")
	}
	in.Ticket = ticket
	in.RunID = "run_interrupted"
	os.Mkdir(filepath.Join(p.RunsDir, in.RunID), 0700)
	if _, err := p.analyzeEngineering(context.Background(), in); err == nil {
		t.Fatal("interrupted attempt was retried automatically")
	}
	t.Run("advisory tokens do not cancel", func(t *testing.T) {
		warn := in
		warn.RunID = "run_advisory"
		warn.Budget.MaxInputTokens = 1
		warn.Budget.MaxOutputTokens = 1
		warn.Budget.WarnTotalTokens = 1
		report, err := p.analyzeEngineering(context.Background(), warn)
		if err != nil || len(report.Alerts) != 3 || report.Usage.InputTokens != 12 {
			t.Fatalf("warning rejected report: %+v %v", report, err)
		}
		if _, err = os.Stat(filepath.Join(p.RunsDir, warn.RunID, "alerts.jsonl")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("tool execution remains rejected", func(t *testing.T) {
		toolScript := filepath.Join(dir, "fake-tool-codex")
		toolCode := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"item.started\",\"item\":{\"type\":\"command_execution\"}}'\n"
		if err := os.WriteFile(toolScript, []byte(toolCode), 0700); err != nil {
			t.Fatal(err)
		}
		p.CLI = toolScript
		in.RunID = "run_tool_event"
		if _, err := p.analyzeEngineering(context.Background(), in); err == nil {
			t.Fatal("tool execution accepted")
		}
		auditData, err := os.ReadFile(filepath.Join(p.RunsDir, in.RunID, "audit.json"))
		if err != nil {
			t.Fatal(err)
		}
		var audit struct {
			UnexpectedToolEvent bool `json:"unexpected_tool_event"`
		}
		if err := json.Unmarshal(auditData, &audit); err != nil || !audit.UnexpectedToolEvent {
			t.Fatalf("tool rejection was not recorded: %s", auditData)
		}
	})
}

func TestOnlyRegisteredReplayToolsAreAllowed(t *testing.T) {
	if !allowedItem("mcp_tool_call", "esa_replay", "run", true) {
		t.Fatal("registered tool blocked")
	}
	for _, c := range []struct {
		kind, server, tool string
		enabled            bool
	}{
		{"command_execution", "", "", true}, {"file_change", "", "", true}, {"mcp_tool_call", "other", "run", true},
		{"mcp_tool_call", "esa_replay", "edit_code", true}, {"mcp_tool_call", "esa_replay", "run", false},
	} {
		if allowedItem(c.kind, c.server, c.tool, c.enabled) {
			t.Fatalf("unexpected capability accepted: %+v", c)
		}
	}
}

func TestCLIHasNoToolOrSessionReuse(t *testing.T) {
	args := strings.Join(CLIArgs("configured-model", "high", "/tmp/isolated"), " ")
	for _, required := range []string{"--ephemeral", "--ignore-user-config", "--sandbox read-only", "features.shell_tool=false", "features.apps=false", "features.plugins=false", "features.multi_agent=false", "web_search=\"disabled\"", "project_doc_max_bytes=0"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing isolation option %s", required)
		}
	}
}

func TestFingerprintNormalizesRedmineLineEndingsOnly(t *testing.T) {
	a := esa.Ticket{ProjectID: "1", Description: "Observed\nstack"}
	b := a
	b.Description = "Observed\r\nstack"
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("Redmine line ending conversion invalidates identical evidence")
	}
	b.Description = "Observed\r\nfuture fix"
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("changed text did not invalidate evidence")
	}
}

func TestMissingUsageRemainsUnknown(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"input_tokens":8}`, `{"input_tokens":null,"output_tokens":0}`, `{"input_tokens":-1,"output_tokens":2}`} {
		if _, _, ok := reportedTokens(json.RawMessage(raw)); ok {
			t.Fatalf("invalid usage recorded as measured: %s", raw)
		}
	}
	if input, output, ok := reportedTokens(json.RawMessage(`{"input_tokens":0,"output_tokens":0}`)); !ok || input != 0 || output != 0 {
		t.Fatal("measured zero usage must remain distinguishable from unknown")
	}
}
