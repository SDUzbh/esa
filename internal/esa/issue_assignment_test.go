package esa

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueCreationWaitsForAcceptedAnalysis(t *testing.T) {
	e := testEngine(t)
	r := receive(t, e, "first", fixture(e, "first"))
	s := e.Store.Snapshot()
	run := s.Runs.Get(r.RunID)
	if s.Issues.Len() != 0 || run.IssueID != "" || s.Investigations.Get(run.InvestigationID).Contract.IssueID != "" {
		t.Fatal("intake created or assigned an issue")
	}
	if _, err := e.ReviewMembership("first", MembershipReview{ExpectedSnapshotID: run.SnapshotID, Reviewer: "engineer", Reason: "Premature split"}); !errors.Is(err, ErrConflict) {
		t.Fatal("membership review created an issue before analysis", err)
	}
	if _, err := e.ProcessOne(context.Background(), nil); err != nil || e.Store.Snapshot().Issues.Len() != 0 {
		t.Fatal("missing provider created an issue", err)
	}
	later := e.Now().Add(time.Hour)
	e.Now = func() time.Time { return later }
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, input AnalyzeRequest) (Report, error) {
		if input.Contract.IssueID != "" || len(inputIssueRecords(t, e, input)) != 0 {
			t.Fatal("task was preclassified")
		}
		report := diagnosticReport()
		report.Issue = &IssueAssignment{Title: "Agent identified empty assignment", Reason: "Inspected assignment dereference"}
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	run = s.Runs.Get(r.RunID)
	issue := s.Issues.Get(run.IssueID)
	if s.Issues.Len() != 1 || issue.Title != "Agent identified empty assignment" || !issue.CreatedAt.Equal(later) || s.Investigations.Get(run.InvestigationID).Contract.IssueID != "" {
		t.Fatal("accepted result did not assign an issue at publication time", issue)
	}
}

func TestUnsuccessfulAnalysisCreatesNoIssue(t *testing.T) {
	for _, name := range []string{"provider_failure", "missing_information", "missing_assignment", "invalid_identity", "superseded"} {
		t.Run(name, func(t *testing.T) {
			e := testEngine(t)
			ticket := fixture(e, "ticket")
			r := receive(t, e, "first", ticket)
			if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
				report := diagnosticReport()
				switch name {
				case "provider_failure":
					return report, errors.New("provider unavailable")
				case "missing_information":
					return Report{Summary: "Need a reproduction", Limitations: []string{"Insufficient evidence"}}, nil
				case "missing_assignment":
					report.Issue = nil
				case "invalid_identity":
					report.Issue = &IssueAssignment{ExistingID: "invented", Title: "Bad assignment", Reason: "Unsupported"}
				case "superseded":
					ticket.Description += " New diagnostic evidence"
					receive(t, e, "changed", ticket)
				}
				return report, nil
			})); err != nil {
				t.Fatal(err)
			}
			s := e.Store.Snapshot()
			if s.Issues.Len() != 0 || s.Solutions.Len() != 0 || s.Runs.Get(r.RunID).IssueID != "" {
				t.Fatal("unsuccessful work created an issue or proposal")
			}
		})
	}
}

func TestCandidateIssueMustRemainVisibleWhenResultArrives(t *testing.T) {
	e := testEngine(t)
	first := fixture(e, "first")
	a := receive(t, e, "first", first)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	old := e.Store.Snapshot().Runs.Get(a.RunID)
	second := fixture(e, "second")
	second.Description += " Another reproduction"
	b := receive(t, e, "second", second)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, input AnalyzeRequest) (Report, error) {
		if len(inputIssueRecords(t, e, input)) != 1 || inputIssueRecords(t, e, input)[0].Summary == "" || inputIssueRecords(t, e, input)[0].RunID != a.RunID {
			t.Fatal("existing diagnosis was not supplied")
		}
		first.Private = true
		receive(t, e, "private", first)
		report := diagnosticReport()
		report.Issue = &IssueAssignment{ExistingID: old.IssueID, Title: "Existing issue", Reason: "Candidate match"}
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if s.Runs.Get(b.RunID).State != "failed" || s.Runs.Get(b.RunID).IssueID != "" || s.Issues.Len() != 0 {
		t.Fatal("withdrawn candidate was assigned or replaced by a fabricated issue")
	}
}

func TestFutureIssuePublicationDoesNotEnterEarlierTask(t *testing.T) {
	e := testEngine(t)
	base := e.Now()
	receive(t, e, "first", fixture(e, "first"))
	later := base.Add(time.Hour)
	e.Now = func() time.Time { return later }
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	ticket := fixture(e, "earlier")
	ticket.CreatedAt, ticket.UpdatedAt = base, base
	ticket.Description += " Earlier independent report"
	r, err := e.Receive(Event{ID: "earlier", Origin: "test", ForwardedBy: "human", ForwardedAt: base, Ticket: ticket})
	if err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	task := s.Investigations.Get(s.Runs.Get(r.RunID).InvestigationID)
	if len(issueRecords(s, task, ticket)) != 0 {
		t.Fatal("future analysis leaked into candidate context")
	}
}

func TestReusedConclusionRetainsSourceVisibilityAndWithdrawal(t *testing.T) {
	e := testEngine(t)
	first := fixture(e, "source")
	receive(t, e, "source", first)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	second := fixture(e, "consumer")
	second.Description += " Another customer report"
	r := receive(t, e, "consumer", second)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, input AnalyzeRequest) (Report, error) {
		return reuseFirstIssue(t, e, input), nil
	})); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	task := s.Investigations.Get(s.Runs.Get(r.RunID).InvestigationID)
	if s.Runs.Get(r.RunID).SourceTicketID != first.ID || task.Application == nil || task.Application.SourceTicketID != first.ID || s.Solutions.Len() != 1 {
		t.Fatal("reuse lost its original evidence source")
	}
	for _, path := range []string{"/v1/tickets/consumer", "/v1/investigations/" + task.ID, "/v1/investigations/" + task.ID + "/solutions"} {
		w := classifierRequest(e, "GET", path, "", "1", "consumer")
		if w.Code != 200 || strings.Contains(w.Body.String(), `"quote":`) || strings.Contains(w.Body.String(), `"source_ticket_id":"source"`) {
			t.Fatal("reused evidence crossed source visibility", path, w.Body)
		}
	}
	first.Private = true
	receive(t, e, "private", first)
	s = e.Store.Snapshot()
	task = s.Investigations.Get(task.ID)
	if task.State != "withdrawn" || task.Application == nil || task.Application.State != "withdrawn" {
		t.Fatal("source privacy failed to withdraw reused proposal")
	}
}

func TestReplacementTaskDoesNotPreassignOrRetainOldMembership(t *testing.T) {
	e := testEngine(t)
	ticket := fixture(e, "ticket")
	first := receive(t, e, "first", ticket)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	oldIssue := e.Store.Snapshot().Runs.Get(first.RunID).IssueID
	r, err := e.Receive(Event{ID: "force", Origin: "test", Force: true, ForwardedBy: "human", ForwardedAt: e.Now(), Ticket: ticket})
	if err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if s.Runs.Get(r.RunID).IssueID != "" || s.Issues.Len() != 1 {
		t.Fatal("replacement task was classified at intake")
	}
	for _, link := range s.Memberships.All() {
		if link.TicketID == ticket.ID && link.State == "tentative" && link.IssueID != "" {
			t.Fatal("old issue membership remained active")
		}
	}
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, input AnalyzeRequest) (Report, error) {
		report := diagnosticReport()
		report.Issue.ExistingID = oldIssue
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	if s.Runs.Get(r.RunID).IssueID != oldIssue || s.Issues.Len() != 1 {
		t.Fatal("existing issue was duplicated after analysis")
	}
}

func TestAgentReceivesIssuesAcrossScopesWithoutHostRelevanceGate(t *testing.T) {
	e := testEngine(t)
	source := fixture(e, "source")
	first := receive(t, e, "source", source)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	original := e.Store.Snapshot().Runs.Get(first.RunID)
	query := fixture(e, "query")
	query.Subject, query.Description = "Unrelated wording", "Another failure report"
	query.Components = []string{"broker"}
	query.Versions = []string{"4.0.0"}
	query.Environment = map[string]string{"details": "JDK 8"}
	receipt := receive(t, e, "query", query)
	calls := 0
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		calls++
		if len(inputIssueRecords(t, e, in)) != 1 {
			t.Fatal("host filtered issue relevance", inputIssueRecords(t, e, in))
		}
		known := inputIssueRecords(t, e, in)[0]
		if known.ID != original.IssueID || known.Versions[0] != "3.7.0" || known.Components[0] != "consumer" || known.Environment["details"] != "JDK 17" {
			t.Fatal("original scope metadata lost", known)
		}
		// The Agent decides this scope has a separate defect despite the supplied issue.
		report := diagnosticReport()
		report.Issue = &IssueAssignment{Title: "Different issue", Reason: "Different failure supported by analysis"}
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if calls != 1 || s.Issues.Len() != 2 || s.Runs.Get(receipt.RunID).IssueID == original.IssueID {
		t.Fatal("Agent assignment was not honored")
	}
}
