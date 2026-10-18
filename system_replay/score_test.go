package systemreplay

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScoreMapsFindingsOntoTheReferencedFixOnly(t *testing.T) {
	g, base, fix := history(t)
	parent, e := g.run("rev-parse", fix+"^")
	if e != nil {
		t.Fatal(e)
	}
	parent = strings.TrimSpace(parent)
	patch, e := g.run("diff", "--unified=0", parent, fix)
	if e != nil {
		t.Fatal(e)
	}
	trace := &Trace{Candidates: []Candidate{
		{Commit: fix, Parent: parent, Reasons: []string{"Ticket reference: BUG-1"}, Changes: append(changes(patch),
			Change{OldFile: "src/test/java/FixTest.java", NewFile: "src/test/java/FixTest.java", OldStart: 1, OldCount: 1})},
		// Found only because a finding named the file: never ground truth.
		{Commit: fix, Parent: parent, Reasons: []string{"Issue file changed: readme"},
			Changes: []Change{{OldFile: "readme", NewFile: "readme", OldStart: 1, OldCount: 1}}},
	}}
	usage := map[string]any{"input_tokens": 1000.0, "output_tokens": 50.0, "calls": 2.0}
	event := func(route string, findings ...Finding) []EventResult {
		return []EventResult{{ElapsedMS: 4000, Observation: Observation{State: "awaiting_review", Route: route, Findings: findings, Usage: usage}}}
	}
	summary := Summary{Results: []Result{
		// Read at the base commit, before an insertion and a rename: old.java:2 is the fix's new.java:3.
		{ID: "BUG-1", Trace: trace, Events: event("analyze",
			Finding{Commit: base, File: "old.java", StartLine: 2, EndLine: 2}, Finding{Commit: base, File: "readme", StartLine: 1, EndLine: 1})},
		{ID: "BUG-2", Trace: trace, Events: event("analyze", Finding{Commit: base, File: "old.java", StartLine: 3, EndLine: 3})},
		{ID: "BUG-3", Trace: &Trace{}, Events: event("analyze")},
		{ID: "DUP-1", Events: event("analyze")},
		{ID: "ASK-1", Events: event("return_to_support")},
		{ID: "EXTRA", Events: event("analyze")},
	}}
	labels := Labels{Seed: 7, Jobs: map[string]JobLabel{
		"BUG-1": {ExpectedRoute: "analyze"}, "BUG-2": {ExpectedRoute: "analyze"}, "BUG-3": {ExpectedRoute: "analyze"},
		"DUP-1": {ExpectedRoute: "reuse", Original: "BUG-1"}, "ASK-1": {ExpectedRoute: "return_to_support"},
	}}
	s := ScoreRun(t.Context(), summary, labels, g.Repo)
	byID := map[string]TicketScore{}
	for _, ticket := range s.Tickets {
		byID[ticket.ID] = ticket
	}
	one := byID["BUG-1"]
	if !one.GroundTruth || !one.FileHit || !one.LineHit || one.Precision != 0.5 || one.Recall != 1 || len(one.FixFiles) != 1 || one.FixFiles[0] != "new.java" {
		t.Fatalf("mapped finding: %+v", one)
	}
	if two := byID["BUG-2"]; !two.FileHit || two.LineHit || two.Precision != 1 {
		t.Fatalf("right file, wrong lines: %+v", two)
	}
	if three := byID["BUG-3"]; three.GroundTruth || s.NoTruth != 1 || s.Localized != 2 {
		t.Fatalf("ticket without a referenced fix: %+v %d %d", three, s.NoTruth, s.Localized)
	}
	if s.FileHit.Hits != 2 || s.FileHit.N != 2 || s.LineHit.Hits != 1 || s.Precision.Value != 0.75 || s.Recall.Value != 1 {
		t.Fatalf("aggregates: %+v %+v %+v %+v", s.FileHit, s.LineHit, s.Precision, s.Recall)
	}
	if s.Routing.Hits != 4 || s.Routing.N != 5 || s.Confusion["reuse"]["analyze"] != 1 || s.RoutingBy["return_to_support"].Hits != 1 {
		t.Fatalf("routing: %+v %v", s.Routing, s.Confusion)
	}
	if len(s.Unlabeled) != 1 || s.Unlabeled[0] != "EXTRA" || s.InputTokens.Total != 5000 || s.ElapsedMS.Median != 4000 || s.Calls.Mean != 2 {
		t.Fatalf("cost: %+v %+v %v", s.InputTokens, s.ElapsedMS, s.Unlabeled)
	}
	dir := filepath.Join(t.TempDir(), "report")
	if e := WriteScore(dir, s); e != nil {
		t.Fatal(e)
	}
	if e := WriteScore(dir, s); e == nil {
		t.Fatal("an existing report directory was overwritten")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "report.json"))
	var back Score
	if e := json.Unmarshal(raw, &back); e != nil || len(back.Tickets) != 5 {
		t.Fatalf("report.json: %v %d", e, len(back.Tickets))
	}
	md, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	for _, want := range []string{"Routing accuracy | 80.0% (4/5", "| reuse | analyze | 1 |", "| BUG-3 | analyze | analyze | 0 | – |"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("report.md lacks %q:\n%s", want, md)
		}
	}
}

func TestIntervals(t *testing.T) {
	// Reference values: Wilson 95% for 8/10 is 0.4902–0.9433.
	r := wilson(8, 10)
	if math.Abs(r.Low-0.4902) > 1e-4 || math.Abs(r.High-0.9433) > 1e-4 {
		t.Fatalf("wilson: %+v", r)
	}
	if r := wilson(0, 0); r.N != 0 || r.Value != 0 {
		t.Fatalf("empty: %+v", r)
	}
	if r := wilson(10, 10); r.High != 1 || r.Low < 0.69 {
		t.Fatalf("all hits: %+v", r)
	}
	values := []float64{0, 1, 1, 0, 1, 0.5, 1, 1}
	m := bootstrap(values, 3)
	if m != bootstrap(values, 3) || m.Value != 0.6875 || m.Low >= m.Value || m.High <= m.Value || m.Low < 0 || m.High > 1 {
		t.Fatalf("bootstrap: %+v", m)
	}
	if s := spread([]float64{5, 1, 3, 2, 4, 6, 7, 8, 9, 10}); s.Median != 6 || s.P90 != 9 || s.Total != 55 {
		t.Fatalf("spread: %+v", s)
	}
	for name, want := range map[string]bool{
		"clients/src/test/java/a/FooTest.java": true, "core/src/main/scala/A.scala": false,
		"tests/kafkatest/x.py": true, "src/main/java/Tests.java": true,
	} {
		if testFile(name) != want {
			t.Fatalf("testFile(%q) != %v", name, want)
		}
	}
}
