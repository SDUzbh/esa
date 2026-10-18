package esa

import (
	"context"
	"testing"
	"time"
)

// advance moves the engine clock forward and runs one retention pass.
func advance(t *testing.T, e *Engine, d time.Duration) {
	t.Helper()
	at := e.Now().Add(d)
	e.Now = func() time.Time { return at }
	if err := e.update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// A solved Issue retires after the retirement period without a match, stays
// in the directory's archive, and is hidden from the default search.
func TestSolvedIssueRetiresToTheArchive(t *testing.T) {
	e := testEngine(t)
	run, _ := closedCase(t, e, true)
	advance(t, e, 179*24*time.Hour)
	if !e.Store.Snapshot().Issues.Get(run.IssueID).RetiredAt.IsZero() {
		t.Fatal("a solved Issue retired before the retirement period")
	}
	advance(t, e, 2*24*time.Hour)
	s := e.Store.Snapshot()
	issue := s.Issues.Get(run.IssueID)
	if issue.ID == "" || !issue.RetiredAt.Equal(e.Now()) {
		t.Fatal("a solved Issue unmatched for 181 days was not retired", issue)
	}
	records := issueRecords(s, Investigation{Contract: TaskContract{ObservationCutoff: e.Now()}}, fixture(e, "query"))
	retired := 0
	for _, r := range records {
		if r.IssueID == run.IssueID {
			if !r.Retired {
				t.Fatal("a retired Issue's record stayed active", r.Kind)
			}
			retired++
		}
	}
	if retired == 0 {
		t.Fatal("a retired Issue's knowledge left the directory")
	}
	if _, _, err := ExportIssueDirectory(t.TempDir(), "1", e.Now(), records); err != nil {
		t.Fatal("a directory with retired records could not be exported", err)
	}
}

func TestRetirementNeedsReviewedKnowledgeAndNoOpenMember(t *testing.T) {
	e := testEngine(t)
	_, unreviewed := analyzedTicket(t, e, "unreviewed")
	ticket, run := analyzedTicket(t, e, "open-source")
	task := e.Store.Snapshot().Investigations.Get(run.InvestigationID)
	review := SolutionReview{ExpectedSolutionID: task.SolutionID, Action: "approve", Reviewer: "engineer", Resolution: Resolution{Cause: "c", Solution: "s", Validation: "v"}}
	if _, err := e.ReviewSolution(task.ID, review); err != nil {
		t.Fatal(err)
	}
	advance(t, e, 365*24*time.Hour)
	s := e.Store.Snapshot()
	if !s.Issues.Get(unreviewed.IssueID).RetiredAt.IsZero() {
		t.Fatal("an Issue without reviewed knowledge was retired")
	}
	if s.Tickets.Get(ticket.ID).Closed || !s.Issues.Get(run.IssueID).RetiredAt.IsZero() {
		t.Fatal("an Issue with an open member ticket was retired")
	}
}

func TestArchiveSearchIsSeparateFromTheActiveDirectory(t *testing.T) {
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []IssueRecord{
		{ID: "active", IssueID: "issue_active", ProjectID: "1", ScopeHash: "s", Kind: "engineer_resolved", Title: "Rebalance stalls", Summary: "Rebalance stalls on partition handoff", Recommendations: []string{"r"}, AvailableAt: at},
		{ID: "old", IssueID: "issue_old", ProjectID: "1", ScopeHash: "s", Kind: "engineer_resolved", Title: "Rebalance stalls", Summary: "Rebalance stalls on partition handoff", Recommendations: []string{"r"}, AvailableAt: at, Retired: true},
	}
	root := t.TempDir()
	relative, digest, err := ExportIssueDirectory(root, "1", at, records)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := OpenIssueDirectory(root, relative, digest)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "rebalance"} {
		active, total, err := directory.Search(query, "", 0, 20)
		if err != nil || total != 1 || active[0].IssueID != "issue_active" || active[0].Retired {
			t.Fatalf("active search %q returned %v (%d, %v)", query, active, total, err)
		}
		archived, total, err := directory.SearchArchive(query, "", 0, 20)
		if err != nil || total != 1 || archived[0].IssueID != "issue_old" || !archived[0].Retired {
			t.Fatalf("archive search %q returned %v (%d, %v)", query, archived, total, err)
		}
	}
	// Records of active Issues keep their bytes, so existing views are unchanged.
	if issueRecordHash(records[0]) != issueRecordHash(IssueRecord{ID: "active", IssueID: "issue_active", ProjectID: "1", ScopeHash: "s", Kind: "engineer_resolved", Title: "Rebalance stalls", Summary: "Rebalance stalls on partition handoff", Recommendations: []string{"r"}, AvailableAt: at}) {
		t.Fatal("the archive field changed an active record's hash")
	}
}

// A ticket matching a retired Issue revives it and is flagged as a possible
// regression; other tickets are not.
func TestMatchRevivesRetiredIssueAsPossibleRegression(t *testing.T) {
	e := testEngine(t)
	run, _ := closedCase(t, e, true)
	advance(t, e, 181*24*time.Hour)
	if e.Store.Snapshot().Issues.Get(run.IssueID).RetiredAt.IsZero() {
		t.Fatal("setup: the solved Issue did not retire")
	}
	again := fixture(e, "again")
	again.Description += " Seen again after the fix"
	r := receive(t, e, "again", again)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		directory, err := OpenIssueDirectory(e.IssueDirectory, in.IssueDirectory, in.IssueDirectoryHash)
		if err != nil {
			return Report{}, err
		}
		if page, _, err := directory.Search("", "", 0, 20); err != nil || len(page) != 0 {
			t.Error("the retired Issue appeared in the default search", page, err)
		}
		page, _, err := directory.SearchArchive("", "engineer_resolved", 0, 1)
		if err != nil || len(page) != 1 {
			t.Fatal("the retired Issue is missing from the archive", err)
		}
		record, err := directory.ReadRecord(page[0].Path, page[0].SHA256)
		if err != nil {
			return Report{}, err
		}
		return ReusedIssueReport(record, RoutingResult{Action: "reuse", RecordPath: page[0].Path, RecordHash: page[0].SHA256, Reason: "Same symptom after the fixed release", Usage: Usage{Calls: 1, InputTokens: 5, OutputTokens: 3, TokensReported: true}}), nil
	})); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	issue := s.Issues.Get(run.IssueID)
	if !issue.RetiredAt.IsZero() || !issue.RevivedAt.Equal(e.Now()) {
		t.Fatal("the matched Issue was not revived", issue)
	}
	link, ok := activeMembership(s, "again")
	if !ok || link.IssueID != run.IssueID || !link.PossibleRegression {
		t.Fatal("the returning ticket was not flagged as a possible regression", link)
	}
	if s.Runs.Get(r.RunID).IssueID != run.IssueID {
		t.Fatal("the run was not assigned to the revived Issue")
	}
	if old, _ := activeMembership(s, "case-source"); old.PossibleRegression {
		t.Fatal("the original member was flagged as a regression")
	}
}
