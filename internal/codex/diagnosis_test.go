package codex

import (
	"context"
	"encoding/json"
	"esa/internal/codexreview"
	"esa/internal/esa"
	"esa/internal/replaytools"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func selectedFinding(commit, file string, line int, text, problem, solution string) EngineeringFinding {
	id := replaytools.EvidenceID("git", commit+":"+file+":", line, text)
	return EngineeringFinding{Location: id, EndLocation: id, Problem: problem, Solution: solution, EvidenceIDs: []string{}}
}

func TestStructuredLocationMustBeHistoricalAndInspected(t *testing.T) {
	packet := Packet{Commit: "old", Documents: []Document{{Source: "git", Prefix: "old:x.java:", First: 2, Lines: []string{"bug();", "tail();"}}}}
	f := selectedFinding("old", "x.java", 2, "bug();", "Null completion", "Fail future")
	out := EngineeringOutput{IssueRequest: esa.IssueRequest{Title: "Completion failure", Reason: "New issue"}, Findings: []EngineeringFinding{f}}
	if r, e := out.Report(packet); e != nil || r.Findings[0].StartLine != 2 || r.Evidence[0].Quote != "bug();" {
		t.Fatal(r, e)
	}
	out.Findings[0].Location = replaytools.EvidenceID("git", "old:x.java:", 1, "bug();")
	if _, e := out.Report(packet); e == nil {
		t.Fatal("uninspected location accepted")
	}
	out.Findings[0] = f
	out.Findings[0].Location = replaytools.EvidenceID("git", "future:x.java:", 2, "bug();")
	if _, e := out.Report(packet); e == nil {
		t.Fatal("future source accepted")
	}
	out.Findings[0] = f
	out.Findings = nil
	if _, e := out.Report(packet); e != nil {
		t.Fatal("unlocated diagnosis must remain possible", e)
	}
}

func TestEvidenceSelectionsResolveAbsoluteLinesWithoutModelCoordinates(t *testing.T) {
	lines := make([]string, 129)
	for i := range lines {
		lines[i] = "// inspected source"
	}
	lines[350-225] = "            complete(response);"
	doc := Document{Source: "git", Prefix: "old:Client.java:", First: 225, Lines: lines}
	view := replaytools.Numbered(doc)
	packet := Packet{Commit: "old", Documents: []Document{doc, {Source: "replay", Prefix: "run:", First: 1, Lines: []string{"CONTROL=PASS"}}}}
	location := view.Lines[350-225].ID
	out := EngineeringOutput{IssueRequest: esa.IssueRequest{Title: "Completion failure", Reason: "Observed"}, Findings: []EngineeringFinding{{Location: location, EndLocation: location, Problem: "Wrong completion", Solution: "Fail on disconnect", EvidenceIDs: []string{replaytools.EvidenceID("replay", "run:", 1, "CONTROL=PASS")}}}}
	r, err := out.Report(packet)
	if err != nil || r.Findings[0].StartLine != 350 || r.Findings[0].EndLine != 350 || r.Evidence[0].Reference != "old:Client.java:350" || len(r.Evidence) != 2 {
		t.Fatal(r, err)
	}
	// Overlapping reads retain IDs; neither window offsets nor EOF affect them.
	if replaytools.Numbered(Document{Source: doc.Source, Prefix: doc.Prefix, First: 350, Lines: []string{lines[125]}}).Lines[0].ID != location {
		t.Fatal("overlapping read changed evidence identity")
	}
	out.Findings[0].EvidenceIDs = []string{"e_invented"}
	if _, err := out.Report(packet); err == nil {
		t.Fatal("unknown supporting evidence accepted")
	}
	out.Findings[0].EvidenceIDs = nil
	out.Findings[0].Location = replaytools.EvidenceID("replay", "run:", 1, "CONTROL=PASS")
	if _, err := out.Report(packet); err == nil {
		t.Fatal("runtime log used as a code location")
	}
	var decoded EngineeringOutput
	if decodeAgentOutput([]byte(`{"issue_request":{"title":"x","reason":"x"},"findings":[{"location":"x","end_location":"x","problem":"x","solution":"x","evidence_ids":[],"start_line":387}],"limitations":[]}`), &decoded) == nil {
		t.Fatal("model-supplied source coordinates accepted")
	}
}

func TestRetrospectiveProviderUsesSeparateToolsAndRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	request := codexreview.ReviewRequest{SourceKey: "BUG-1", Baseline: "old", Candidate: codexreview.Candidate{Commit: "fix", PRs: []string{"#1"}}, Prediction: esa.Report{Findings: []esa.Finding{{Commit: "old", File: "x.java", StartLine: 2, EndLine: 2}}}, Documents: []codexreview.Document{{Source: "patch", Prefix: "fix:patch:", First: 1, Lines: []string{"+ fail(disconnected);"}}}}
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

func TestDiagnosisSchemaDoesNotAskForClassification(t *testing.T) {
	schema := string(EngineeringSchema())
	for _, field := range []string{"classification", "categories", "tags", "issue", "existing_id", "assignment_routing"} {
		if strings.Contains(schema, `"`+field+`"`) {
			t.Fatal("classification remains in active schema", schema)
		}
	}
}

func TestEngineeringCannotEmitAnIssueAssignment(t *testing.T) {
	for _, raw := range []string{
		`{"issue":{"existing_id":"issue_other","title":"Cause","reason":"claim"},"findings":[],"limitations":[]}`,
		`{"issue_request":{"title":"Cause","reason":"evidence","existing_id":"issue_other"},"findings":[],"limitations":[]}`,
		`{"issue_request":{"title":"Cause","reason":"evidence"},"issue":{"existing_id":"issue_other"},"findings":[],"limitations":[]}`,
	} {
		var output EngineeringOutput
		if err := decodeAgentOutput([]byte(raw), &output); err == nil {
			t.Fatal("engineering output bypassed its grouping request contract", raw)
		}
	}
}
