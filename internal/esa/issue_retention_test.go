package esa

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Seed a full catalogue with ordinary ticket associations. Business operations
// below exercise admission, public detail access, review and durable restart.
func seedIssueCatalogue(t *testing.T, e *Engine, count int) {
	t.Helper()
	if err := e.Store.Update(func(s *State) error {
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("issue_%03d", i)
			ticket := fixture(e, fmt.Sprintf("source-%03d", i))
			ticket.ForwardedBy, ticket.ForwardedAt = "human", e.Now()
			s.Tickets.Put(ticket.ID, ticket)
			s.Issues.Put(id, EngineeringIssue{ID: id, ProjectID: ticket.ProjectID, Title: ticket.Subject, TitleSourceTicketID: ticket.ID, CreatedAt: e.Now().Add(time.Duration(i-count) * time.Second)})
			s.Memberships.Put(id, IssueTicketLink{ID: id, IssueID: id, TicketID: ticket.ID, State: "tentative", Version: 1})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyIssueCleanupPreservesAudit(t *testing.T) {
	e := testEngine(t)
	ticket, run := analyzedTicket(t, e, "source")
	before := e.Store.Snapshot()
	link, _ := activeMembership(before, ticket.ID)
	if _, err := e.ReviewMembership(ticket.ID, MembershipReview{ExpectedMembershipID: link.ID, ExpectedSnapshotID: before.LatestSnapshot.Get(ticket.ID), Reviewer: "engineer", Reason: "Different cause"}); err != nil {
		t.Fatal(err)
	}
	after := e.Store.Snapshot()
	if _, exists := after.Issues.Lookup(run.IssueID); exists || after.Issues.Len() != 1 {
		t.Fatal("empty former issue was not removed")
	}
	if hash(before.Runs) != hash(after.Runs) || hash(before.Solutions) != hash(after.Solutions) || hash(before.Snapshots) != hash(after.Snapshots) {
		t.Fatal("cleanup altered immutable diagnostic history")
	}
	if len(issueRecords(after, Investigation{Contract: TaskContract{ObservationCutoff: e.Now()}}, fixture(e, "query"))) != 0 {
		t.Fatal("removed issue reappeared through historical knowledge")
	}
	if _, err := e.ReviewSolution(run.InvestigationID, SolutionReview{ExpectedSolutionID: before.Investigations.Get(run.InvestigationID).SolutionID, Action: "withdraw", Reviewer: "engineer"}); err == nil {
		t.Fatal("removed issue accepted a new knowledge review")
	}
}

func TestIssueLimitUsesDurableAccessOrder(t *testing.T) {
	e := testEngine(t)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e.Store = store
	seedIssueCatalogue(t, e, IssueLimit)
	// Legacy issues have no access metadata: creation time provides a stable
	// initial order. A successful detail read makes the oldest one most recent.
	before := hash(e.Store.Snapshot().Issues)
	for _, endpoint := range []string{"/v1/engineering-issues", "/v1/tickets"} {
		if response := classifierRequest(e, "GET", endpoint, "", "1", ""); response.Code != 200 {
			t.Fatal(response.Body)
		}
	}
	if hash(e.Store.Snapshot().Issues) != before {
		t.Fatal("list browsing refreshed LRU")
	}
	if response := classifierRequest(e, "GET", "/v1/tickets/source-000", "", "1", "source-001"); response.Code != 404 {
		t.Fatal("unauthorized detail access succeeded")
	}
	if hash(e.Store.Snapshot().Issues) != before {
		t.Fatal("unauthorized request refreshed LRU")
	}
	if response := classifierRequest(e, "GET", "/v1/tickets/source-000", "", "1", "source-000"); response.Code != 200 {
		t.Fatal(response.Body)
	}
	seen := e.Store.Snapshot().Issues.Get("issue_000")
	if seen.AccessOrder == 0 || !seen.LastAccessedAt.Equal(e.Now()) {
		t.Fatal("detail access was not recorded")
	}
	ticket := fixture(e, "new")
	ticket.ProjectID = "2" // The limit is global, not a separate project quota.
	ticket.Description += " independent new issue"
	receipt := receive(t, e, "new", ticket)
	if _, err = e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	if state.Issues.Len() != IssueLimit || state.Issues.Get("issue_000").ID == "" || state.Issues.Get("issue_001").ID != "" || state.Runs.Get(receipt.RunID).IssueID == "" {
		t.Fatal("global limit did not evict the least recently accessed issue")
	}
	if state.Memberships.Get("issue_001").State != "detached" {
		t.Fatal("eviction left an active membership")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(reopened, e.Config)
	if err != nil {
		t.Fatal(err)
	}
	engine.Now = e.Now
	if engine.Store.Snapshot().Issues.Get("issue_000").AccessOrder != seen.AccessOrder {
		t.Fatal("restart lost LRU order")
	}
	response := classifierRequest(engine, "GET", "/v1/tickets/source-000", "", "1", "source-000")
	if response.Code != 200 || engine.Store.Snapshot().Issues.Get("issue_000").AccessOrder <= state.IssueAccessSequence {
		t.Fatal("restart reset access sequence", response.Body)
	}
}

func TestStartupPrunesEmptyAndExcessIssues(t *testing.T) {
	e := testEngine(t)
	seedIssueCatalogue(t, e, IssueLimit+3)
	if err := e.Store.Update(func(s *State) error {
		s.Issues.Put("empty", EngineeringIssue{ID: "empty", CreatedAt: e.Now()})
		s.IssueOverrides.Put("source-000", "issue_000")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngine(e.Store, e.Config); err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	if state.Issues.Len() != IssueLimit || state.Issues.Get("empty").ID != "" || state.Issues.Get("issue_002").ID != "" || state.Issues.Get("issue_003").ID == "" || state.IssueOverrides.Get("source-000") != "" {
		t.Fatal("startup retention failed")
	}
}

func TestLRUEvictionCannotResurrectLinkedCase(t *testing.T) {
	e := testEngine(t)
	ticket, run := analyzedTicket(t, e, "case-source")
	ticket.Closed = true
	ticket.Resolution = Resolution{Cause: "cause", Solution: "repair", Validation: "regression"}
	closed := receive(t, e, "closed", ticket)
	if _, err := e.ReviewCase(closed.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().Issues.Get(run.IssueID).ID == "" {
		t.Fatal("closed case was incorrectly considered empty")
	}
	seedIssueCatalogue(t, e, IssueLimit)
	if err := e.update(func(s *State) error {
		// Make all new issues more recent, at the same replay timestamp.
		for i := 0; i < IssueLimit; i++ {
			id := fmt.Sprintf("issue_%03d", i)
			issue := s.Issues.Get(id)
			issue.CreatedAt = e.Now()
			s.Issues.Put(id, issue)
			touchIssue(s, id, e.Now())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	if state.Issues.Len() != IssueLimit || state.Issues.Get(run.IssueID).ID != "" || state.Cases.Get(closed.CaseID).State != "trusted" {
		t.Fatal("capacity eviction changed the retained case audit")
	}
	for _, record := range issueRecords(state, Investigation{Contract: TaskContract{ObservationCutoff: e.Now()}}, fixture(e, "query")) {
		if record.IssueID == run.IssueID {
			t.Fatal("evicted case returned to the catalogue")
		}
	}
	response := classifierRequest(e, "GET", "/v1/tickets/case-source", "", "1", "case-source")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"state":"issue_removed"`) {
		t.Fatal("retired current result was not distinguished from audit history", response.Body)
	}
}

func TestReportedAccessCannotTouchOutsideFrozenDirectory(t *testing.T) {
	e := testEngine(t)
	_, original := analyzedTicket(t, e, "source")
	before := e.Store.Snapshot().Issues.Get(original.IssueID).AccessOrder
	query := fixture(e, "query")
	query.Description += " distinct report"
	receipt := receive(t, e, "query", query)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		report := diagnosticReport()
		report.AccessedIssueIDs = []string{"issue_not_in_directory"}
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	if state.Runs.Get(receipt.RunID).State != "failed" || state.Issues.Get(original.IssueID).AccessOrder != before {
		t.Fatal("invalid provider access changed the catalogue")
	}
	if _, err := e.AnalyzeTicket(query.ID, true, "engineer"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		report := diagnosticReport()
		report.AccessedIssueIDs = []string{original.IssueID}
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().Issues.Get(original.IssueID).AccessOrder <= before {
		t.Fatal("eligible provider read did not update LRU")
	}
}

func TestEvictionDuringAnalysisCannotRestoreIssue(t *testing.T) {
	e := testEngine(t)
	_, original := analyzedTicket(t, e, "source")
	query := fixture(e, "query")
	query.Description += " independent report"
	receipt := receive(t, e, "query", query)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, input AnalyzeRequest) (Report, error) {
		report := reuseFirstIssue(t, e, input)
		seedIssueCatalogue(t, e, IssueLimit)
		if err := e.update(func(s *State) error {
			for i := 0; i < IssueLimit; i++ {
				touchIssue(s, fmt.Sprintf("issue_%03d", i), e.Now())
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	if state.Issues.Len() != IssueLimit || state.Issues.Get(original.IssueID).ID != "" || state.Runs.Get(receipt.RunID).State != "failed" || state.Runs.Get(receipt.RunID).IssueID != "" {
		t.Fatal("stale analysis restored an evicted issue")
	}
}

// An open source ticket's usable analysis keeps its Issue unless the ticket
// is now assigned elsewhere; an override naming the same Issue keeps it too.
func TestUsableAnalysisKeepsIssueUnlessReassigned(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		override string
		kept     bool
	}{{"", true}, {"issue_a", true}, {"issue_b", false}} {
		for _, indexed := range []bool{false, true} {
			var s State
			ticket := Ticket{ID: "t1", ProjectID: "p", Subject: "Broken export"}
			s.Tickets.Put(ticket.ID, ticket)
			s.Runs.Put("r1", Run{ID: "r1", TicketID: ticket.ID, Input: ticket})
			s.Investigations.Put("i1", Investigation{ID: "i1", SolutionID: "s1"})
			s.Solutions.Put("s1", SolutionRevision{ID: "s1", IssueID: "issue_a", InvestigationID: "i1", RunID: "r1", SourceTicketID: ticket.ID, State: "proposed"})
			for _, id := range []string{"issue_a", "issue_b"} {
				s.Issues.Put(id, EngineeringIssue{ID: id, CreatedAt: at})
			}
			s.Memberships.Put("m_b", IssueTicketLink{ID: "m_b", IssueID: "issue_b", TicketID: "t2", State: "tentative"})
			s.Tickets.Put("t2", Ticket{ID: "t2", ProjectID: "p"})
			if tc.override != "" {
				s.IssueOverrides.Put(ticket.ID, tc.override)
			}
			if indexed {
				s.advanceIndexes()
			}
			maintainIssues(&s, at)
			if _, kept := s.Issues.Lookup("issue_a"); kept != tc.kept {
				t.Fatalf("override %q, indexed %v: issue kept = %v, want %v", tc.override, indexed, kept, tc.kept)
			}
		}
	}
}
