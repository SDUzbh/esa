package esa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLaterCaseReviewDoesNotRefreshOrLeakFutureContext(t *testing.T) {
	e := testEngine(t)
	initial := receive(t, e, "initial", fixture(e, "open"))
	before := e.Store.Snapshot()
	initialRun := before.Runs.Get(initial.RunID)
	snapshot := before.Snapshots.Get(initialRun.SnapshotID)
	later := e.Now().Add(time.Hour)
	e.Now = func() time.Time { return later }
	_, closed := resolved(t, e)
	if _, err := e.ReviewCase(closed.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if s.Latest.Get("open") != initialRun.ID || s.Runs.Get(initialRun.ID).State != "queued" || s.Runs.Len() != before.Runs.Len() {
		t.Fatal("case approval replaced the investigation")
	}
	if hash(s.Snapshots.Get(snapshot.ID)) != hash(snapshot) || !s.Runs.Get(initialRun.ID).ObservationCutoff.Equal(initialRun.ObservationCutoff) {
		t.Fatal("review rewrote frozen inputs")
	}
	calls := 0
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		calls++
		if len(inputIssueRecords(t, e, in)) != 0 {
			t.Fatal("future case leaked", inputIssueRecords(t, e, in))
		}
		return diagnosticReport(), nil
	})); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	query := fixture(e, "later-query")
	query.Description += " distinct input"
	next := receive(t, e, "later-forward", query)
	laterState := e.Store.Snapshot()
	if len(issueRecords(laterState, laterState.Investigations.Get(laterState.Runs.Get(next.RunID).InvestigationID), query)) < 1 {
		t.Fatal("new forwarding omitted visible case context")
	}

}

func TestClosingOnlyMemberSupersedesUnstartedInvestigation(t *testing.T) {
	e := testEngine(t)
	ticket := fixture(e, "only-member")
	r := receive(t, e, "open", ticket)
	if _, err := e.ProcessOne(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ticket.Closed = true
	receive(t, e, "closed", ticket)
	s := e.Store.Snapshot()
	run := s.Runs.Get(r.RunID)
	if run.State != "superseded" || string(s.Investigations.Get(run.InvestigationID).State) != string(run.State) {
		t.Fatal("closed ticket retained a runnable investigation", run)
	}
}

func TestCaseReviewDuringAnalysisDoesNotReplaceAgentResult(t *testing.T) {
	e := testEngine(t)
	open := fixture(e, "open")
	r := receive(t, e, "open", open)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		_, closed := resolved(t, e)
		if _, err := e.ReviewCase(closed.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
			t.Fatal(err)
		}
		return diagnosticReport(), nil
	})); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	run := s.Runs.Get(r.RunID)
	if run.State != "awaiting_review" || run.Report.Usage.InputTokens != 20 || s.Solutions.Len() != 1 || s.Latest.Get(open.ID) != r.RunID {
		t.Fatal("late proposal overwrote advice or lost usage", run)
	}
}

func TestFailedAnalysisKeepsUsageAndAlerts(t *testing.T) {
	e := testEngine(t)
	r := receive(t, e, "failure", fixture(e, "failure"))
	e.Config.Budget.WarnTotalTokens = 1
	_, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		return Report{Usage: Usage{Calls: 1, InputTokens: 123, OutputTokens: 456, TokensReported: true}}, errors.New("provider output failed validation")
	}))
	if err != nil {
		t.Fatal(err)
	}
	run := e.Store.Snapshot().Runs.Get(r.RunID)
	if run.State != "failed" || run.Report.Usage.InputTokens != 123 || len(run.Report.Alerts) == 0 || e.Store.Snapshot().Solutions.Len() != 0 {
		t.Fatalf("failed spend or trust changed: %+v", run)
	}
}

func TestHTTPProviderFailureKeepsReportedSpend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad output", "report": Report{Usage: Usage{Calls: 1, InputTokens: 99, TokensReported: true}}})
	}))
	defer server.Close()
	report, err := (HTTPAnalyzer{URL: server.URL}).Analyze(context.Background(), AnalyzeRequest{Budget: Budget{TimeoutSeconds: 10}})
	if err == nil || report.Usage.InputTokens != 99 || !report.Usage.TokensReported {
		t.Fatal("failure transport discarded usage", report, err)
	}
}

func TestAgentAssignmentAndMemberCasePublishOneFamily(t *testing.T) {
	e := testEngine(t)
	first := receive(t, e, "first", fixture(e, "first"))
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	firstRun := e.Store.Snapshot().Runs.Get(first.RunID)
	second := fixture(e, "second")
	second.Description += " A second independently reported reproduction."
	receipt := receive(t, e, "second", second)
	initial := e.Store.Snapshot().Runs.Get(receipt.RunID)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, input AnalyzeRequest) (Report, error) {
		found := false
		for _, issue := range inputIssueRecords(t, e, input) {
			found = found || issue.ID == firstRun.IssueID
		}
		if !found {
			t.Fatal("existing eligible family not supplied")
		}
		report := diagnosticReport()
		report.Issue = &IssueAssignment{ExistingID: firstRun.IssueID, Title: "Empty assignment", Reason: "Matching defect and environment evidence"}
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	run := s.Runs.Get(receipt.RunID)
	task := s.Investigations.Get(run.InvestigationID)
	if s.Issues.Len() != 1 || initial.IssueID != "" || run.IssueID != firstRun.IssueID || task.Contract.IssueID != initial.IssueID || hash(run.Input) != hash(initial.Input) {
		t.Fatal("assignment rewrote frozen input or failed to update membership")
	}
	second.Closed = true
	second.Resolution = Resolution{Cause: "Reviewed shared cause", Solution: "Guard assignment", Validation: "Scoped regression verification"}
	c := receive(t, e, "resolved-member", second)
	if _, err := e.ReviewCase(c.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	for _, task := range s.Investigations.All() {
		if task.IssueID == firstRun.IssueID {
			solution := s.Solutions.Get(task.SolutionID)
			if solution.State != "trusted" || solution.SourceTicketID != second.ID || s.Runs.Get(task.RunID).State != "completed" {
				t.Fatalf("shared reviewed solution diverged: %+v", solution)
			}
		}
	}
}
