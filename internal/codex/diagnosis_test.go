package codex

import (
	"context"
	"encoding/json"
	"esa/internal/codexreview"
	"esa/internal/esa"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStructuredLocationMustBeHistoricalAndInspected(t *testing.T) {
	packet := Packet{Commit: "old", Documents: []Document{{Source: "git", Prefix: "old:x.java:", First: 2, Lines: []string{"bug();", "tail();"}}}}
	f := esa.Finding{Commit: "old", File: "x.java", StartLine: 2, EndLine: 2, Problem: "Null completion", Solution: "Fail future", Evidence: []esa.Evidence{{Source: "git", Reference: "old:x.java:2", Quote: "bug();"}}}
	out := EngineeringOutput{Classification: "code_defect", Issue: esa.IssueAssignment{Title: "Completion failure", Reason: "New issue"}, Findings: []esa.Finding{f}}
	if _, e := out.Report(packet); e != nil {
		t.Fatal(e)
	}
	out.Findings[0].StartLine = 1
	if _, e := out.Report(packet); e == nil {
		t.Fatal("uninspected location accepted")
	}
	out.Findings[0] = f
	out.Findings[0].Commit = "future"
	if _, e := out.Report(packet); e == nil {
		t.Fatal("future source accepted")
	}
	out.Findings[0] = f
	out.Classification = "usage_question"
	if _, e := out.Report(packet); e == nil {
		t.Fatal("support engineering finding accepted")
	}
	out.Findings = nil
	if _, e := out.Report(packet); e != nil {
		t.Fatal("support classification must remain possible", e)
	}
	out.Issue.ExistingID = "unknown"
	if _, e := out.Report(packet); e == nil {
		t.Fatal("invented existing issue accepted")
	}
}

func TestRetrospectiveProviderUsesSeparateToolsAndRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	request := codexreview.ReviewRequest{SourceKey: "BUG-1", Baseline: "old", Candidate: codexreview.Candidate{Commit: "fix", PRs: []string{"#1"}}, Prediction: esa.Report{AgentClassification: &esa.Classification{Category: "code_defect"}, Findings: []esa.Finding{{Commit: "old", File: "x.java", StartLine: 2, EndLine: 2}}}, Documents: []codexreview.Document{{Source: "patch", Prefix: "fix:patch:", First: 1, Lines: []string{"+ fail(disconnected);"}}}}
	answer := map[string]any{"commit": "fix", "verdict": "resolved", "explanation": "The PR fails a disconnected response instead of completing it successfully.", "findings": []codexreview.FindingReview{{Index: 0, Localization: "correct", Problem: "correct", Solution: "correct", Explanation: "The finding describes the completion correction."}}, "evidence": []esa.Evidence{{Source: "patch", Reference: "fix:patch:1", Quote: "fail(disconnected)"}}, "limitations": []string{"Static PR review only."}}
	b, _ := json.Marshal(answer)
	if e := os.WriteFile(fixture, b, 0600); e != nil {
		t.Fatal(e)
	}
	script := filepath.Join(dir, "fake-codex")
	code := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\nif [ \"$1\" = '-o' ]; then cp '" + fixture + "' \"$2\"; fi\nshift\ndone\nprintf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":10}}'\n"
	if e := os.WriteFile(script, []byte(code), 0700); e != nil {
		t.Fatal(e)
	}
	p := Provider{CLI: script, RunsDir: filepath.Join(dir, "runs")}
	r, e := p.Review(context.Background(), request, esa.Budget{MaxCalls: 1, MaxInputTokens: 1, MaxOutputTokens: 1, WarnTotalTokens: 1, TimeoutSeconds: 5}, "run_review")
	if e != nil || r.Verdict != "resolved" || !r.Usage.TokensReported || len(r.Alerts) != 3 {
		t.Fatalf("%+v %v", r, e)
	}
	prompt, _ := os.ReadFile(filepath.Join(p.RunsDir, "run_review", "prompt.txt"))
	if !strings.Contains(string(prompt), "1: + fail(disconnected);") {
		t.Fatal("line numbers missing")
	}
	p.ToolsConfig = "unexpected"
	if _, e = p.Review(context.Background(), request, esa.Budget{}, "run_tools"); e == nil {
		t.Fatal("retrospective tools accepted")
	}
}
