package backtest

import (
	"context"
	"esa/internal/esa"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func history(t *testing.T) (Git, string, string) {
	t.Helper()
	root := t.TempDir()
	n := 0
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		stamp := time.Date(2020, 1, n+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	write := func(file, text string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(root, file), []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
	}
	commit := func(message string) { n++; git("add", "."); git("commit", "-m", message) }
	write("old.java", "a\nbug();\nz\n")
	commit("base")
	base := git("rev-parse", "HEAD")
	git("tag", "1.0")
	write("readme", "unrelated\n")
	commit("Documentation (#10)")
	git("tag", "1.1")
	write("old.java", "insert\na\nbug();\nz\n")
	commit("Move declarations (#11)")
	git("tag", "1.2")
	git("mv", "old.java", "new.java")
	commit("Rename (#12)")
	write("new.java", "insert\na\nfixed();\nz\n")
	commit("BUG-1: Correct completion\n\nCloses #19 from user/fix")
	fix := git("rev-parse", "HEAD")
	git("tag", "1.3")
	write("new.java", "insert\na\nbug();\nz\n")
	commit("Revert BUG-1 (#20)")
	git("tag", "1.4")
	git("checkout", "-b", "unmerged", base)
	write("old.java", "unmerged repair\n")
	commit("BUG-1 (#99)")
	git("checkout", "main")
	return Git{root, context.Background()}, base, fix
}
func TestMainlineScopeVersionsRenameAndUnmerged(t *testing.T) {
	g, base, fix := history(t)
	issue := Issue{ID: "issue_BUG-1", TicketKeys: []string{"BUG-1"}, Findings: []esa.Finding{{Commit: base, File: "old.java", StartLine: 2, EndLine: 2}}}
	trace, e := g.Trace(issue, base, "main", "2020-01-02T00:00:01Z", "", 3)
	if e != nil {
		t.Fatal(e)
	}
	if len(trace.Versions) != 3 || len(trace.Candidates) != 3 {
		t.Fatalf("unexpected graph: %+v", trace)
	}
	found := false
	for _, c := range trace.Candidates {
		if strings.Contains(c.Message, "#99") || strings.Contains(c.Message, "Revert") {
			t.Fatal("unmerged or out-of-window commit included")
		}
		if c.Commit == fix {
			found = true
			if len(c.PRs) != 1 || c.PRs[0] != "#19" {
				t.Fatal(c.PRs)
			}
			if !strings.Contains(strings.Join(c.Reasons, " "), "Issue lines changed: new.java") {
				t.Fatal(c.Reasons)
			}
		}
	}
	if !found {
		t.Fatal("repair after rename was missed")
	}
	later, e := g.Trace(issue, base, "main", "2020-01-04T00:00:01Z", "", 2)
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range later.Candidates {
		if strings.Contains(c.Message, "Move declarations") {
			t.Fatal("pre-receipt change treated as future repair")
		}
	}
	if _, e = g.Trace(issue, fix, "main", "2020-01-02T00:00:01Z", "", 3); e == nil {
		t.Fatal("future baseline accepted")
	}
}
func TestReverseAssessmentAndRegression(t *testing.T) {
	prediction := esa.Report{AgentClassification: &esa.Classification{Category: "code_defect"}, Findings: []esa.Finding{{File: "x.java"}}}
	trace := Trace{Issue: Issue{ID: "issue_1"}, Candidates: []Candidate{{Commit: "fix"}, {Commit: "revert"}}, State: "awaiting_review"}
	r := Review{Commit: "fix", Verdict: "resolved", Findings: []FindingReview{{0, "correct", "correct", "correct", "PR confirms root cause."}}}
	e, err := Evaluate("BUG-1", trace, prediction, []Review{r})
	if err != nil || e.Correct != 1 || e.Pending != 1 || e.State != "resolved" {
		t.Fatalf("%+v %v", e, err)
	}
	e, err = Evaluate("BUG-1", trace, prediction, []Review{{Commit: "revert", Verdict: "regressed"}, r})
	if err != nil || e.State != "reopened" || e.Correct != 1 {
		t.Fatalf("%+v %v", e, err)
	}
	e, err = Evaluate("BUG-1", trace, prediction, nil)
	if err != nil || e.Unknown != 1 {
		t.Fatal(e, err)
	}
	if _, err = Evaluate("BUG-1", trace, prediction, []Review{r, r}); err == nil {
		t.Fatal("duplicate reviews accepted")
	}
}
func TestReviewRejectsUnconfirmedAccuracyAndInventedEvidence(t *testing.T) {
	request := ReviewRequest{Candidate: Candidate{Commit: "fix", PRs: []string{"#1"}}, Prediction: esa.Report{Findings: []esa.Finding{{}}}, Documents: []Document{{"patch", "fix:patch:", 1, []string{"+ fail(disconnect);"}}}}
	r := Review{Commit: "fix", Verdict: "resolved", Explanation: "PR repairs completion.", Findings: []FindingReview{{0, "correct", "correct", "correct", "Verified patch."}}, Evidence: []esa.Evidence{{Source: "patch", Reference: "fix:patch:1", Quote: "fail(disconnect)"}}}
	if e := ValidateReview(r, request); e != nil {
		t.Fatal(e)
	}
	r.Verdict = "uncertain"
	if ValidateReview(r, request) == nil {
		t.Fatal("accuracy inferred without confirmed fix")
	}
	r.Verdict = "resolved"
	r.Evidence[0].Quote = "invented"
	if ValidateReview(r, request) == nil {
		t.Fatal("invented patch quote accepted")
	}
	r.Evidence[0].Quote = "fail(disconnect)"
	request.Candidate.PRs = nil
	if ValidateReview(r, request) == nil {
		t.Fatal("resolution accepted without PR")
	}
}
func TestIssueAssignmentProjectAndCategory(t *testing.T) {
	t1 := esa.Ticket{SourceKey: "BUG-1", ProjectID: "A"}
	p := esa.Report{AgentClassification: &esa.Classification{Category: "code_defect"}, Issue: &esa.IssueAssignment{Title: "Failure", Reason: "New"}}
	items, first, e := Assign(nil, t1, p)
	if e != nil {
		t.Fatal(e)
	}
	p.Issue.ExistingID = first.ID
	t1.SourceKey = "BUG-2"
	items, second, e := Assign(items, t1, p)
	if e != nil || len(items) != 1 || len(second.TicketKeys) != 2 {
		t.Fatal(second, e)
	}
	t1.ProjectID = "B"
	if _, _, e = Assign(items, t1, p); e == nil {
		t.Fatal("cross-project grouping accepted")
	}
}

func TestTicketReferenceInMergedBranchLandsAtMainlineMerge(t *testing.T) {
	g, base, _ := history(t)
	git := func(date string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", g.Repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("2020-01-08T00:00:00Z", "checkout", "-b", "landing", base)
	if e := os.WriteFile(filepath.Join(g.Repo, "other.java"), []byte("repair();\n"), 0600); e != nil {
		t.Fatal(e)
	}
	git("2020-01-08T00:00:00Z", "add", ".")
	git("2020-01-08T00:00:00Z", "commit", "-m", "BUG-2: Repair other path (#55)")
	git("2020-01-09T00:00:00Z", "checkout", "main")
	git("2020-01-09T00:00:00Z", "merge", "--no-ff", "landing", "-m", "Merge implementation branch")
	landing := git("2020-01-09T00:00:00Z", "rev-parse", "HEAD")
	git("2020-01-09T00:00:00Z", "tag", "1.5")
	trace, e := g.Trace(Issue{ID: "BUG-2", TicketKeys: []string{"BUG-2"}}, base, "main", "2020-01-02T00:00:01Z", "", 5)
	if e != nil || len(trace.Candidates) != 1 || trace.Candidates[0].Commit != landing {
		t.Fatalf("%+v %v", trace, e)
	}
	if len(trace.Candidates[0].PRs) != 1 || trace.Candidates[0].PRs[0] != "#55" {
		t.Fatal(trace.Candidates[0])
	}
}
