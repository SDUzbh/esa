package esa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestLaterCaseReviewRefreshesWithoutNewAnalysis(t *testing.T) {
	e := testEngine(t)
	initial := receive(t, e, "initial", fixture(e, "open"))
	before := e.Store.Snapshot()
	initialRun := before.Runs[initial.RunID]
	snapshot := before.Snapshots[initialRun.SnapshotID]
	later := e.Now().Add(time.Hour)
	e.Now = func() time.Time { return later }
	_, closed := resolved(t, e)
	if _, err := e.ReviewCase(closed.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	recommendation := s.Runs[s.Latest["open"]]
	if recommendation.Route != "case_reuse" || !recommendation.ObservationCutoff.Equal(later) || recommendation.Report.Usage.Calls != 0 {
		t.Fatalf("later approved case was not published at its availability: %+v", recommendation)
	}
	if hash(s.Snapshots[snapshot.ID]) != hash(snapshot) || !s.Runs[initialRun.ID].ObservationCutoff.Equal(initialRun.ObservationCutoff) || len(s.Runs[initialRun.ID].Matches) != 0 {
		t.Fatal("later review rewrote original input or supplied future case knowledge")
	}
	if len(s.Investigations) != 1 || s.Investigations[initialRun.InvestigationID].State != "superseded" || s.Runs[initialRun.ID].State != "superseded" {
		t.Fatal("queued investigation and run state diverged after recommendation")
	}
	calls := 0
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		calls++
		return diagnosticReport(), nil
	})); err != nil || calls != 0 {
		t.Fatal("review triggered extra engineering work", calls, err)
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
	run := s.Runs[r.RunID]
	if run.State != "superseded" || s.Investigations[run.InvestigationID].State != run.State {
		t.Fatal("closed ticket retained a runnable investigation", run)
	}
}

func TestReviewDuringAnalysisKeepsSpendWithoutReplacingTrustedAdvice(t *testing.T) {
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
	run := s.Runs[r.RunID]
	if run.State != "superseded" || run.Report.Usage.InputTokens != 20 || len(s.Solutions) != 0 || s.Runs[s.Latest[open.ID]].Route != "case_reuse" {
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
	run := e.Store.Snapshot().Runs[r.RunID]
	if run.State != "failed" || run.Report.Usage.InputTokens != 123 || len(run.Report.Alerts) == 0 || len(e.Store.Snapshot().Solutions) != 0 {
		t.Fatalf("failed spend or trust changed: %+v", run)
	}
}

func TestUnchangedAuthoritySkipsArchiveTransfer(t *testing.T) {
	seed, _ := OpenStore("")
	state := seed.Snapshot()
	revision := uint64(1)
	fullReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == strconv.Quote(strconv.FormatUint(revision, 10)) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fullReads++
		_ = json.NewEncoder(w).Encode(statePacket{State: state, Revision: revision})
	}))
	defer server.Close()
	store, err := OpenBackendStore(HTTPStateBackend{URL: server.URL, Token: "123456789012345678901234"})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Refresh(); err != nil || fullReads != 1 {
		t.Fatal("unchanged archive was transferred", fullReads, err)
	}
	state.Generation = 9
	revision++
	if err = store.Refresh(); err != nil || fullReads != 2 || store.Snapshot().Generation != 9 {
		t.Fatal("changed authority was not refreshed", fullReads, err)
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
	firstRun := e.Store.Snapshot().Runs[first.RunID]
	second := fixture(e, "second")
	second.Description += " A second independently reported reproduction."
	receipt := receive(t, e, "second", second)
	initial := e.Store.Snapshot().Runs[receipt.RunID]
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, input AnalyzeRequest) (Report, error) {
		found := false
		for _, issue := range input.KnownIssues {
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
	run := s.Runs[receipt.RunID]
	task := s.Investigations[run.InvestigationID]
	if run.IssueID != firstRun.IssueID || task.Contract.IssueID != initial.IssueID || hash(run.Input) != hash(initial.Input) {
		t.Fatal("assignment rewrote frozen input or failed to update membership")
	}
	second.Closed = true
	second.Resolution = Resolution{Cause: "Reviewed shared cause", Solution: "Guard assignment", Validation: "Scoped regression verification"}
	c := receive(t, e, "resolved-member", second)
	if _, err := e.ReviewCase(c.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	for _, task := range s.Investigations {
		if task.IssueID == firstRun.IssueID {
			solution := s.Solutions[task.SolutionID]
			if solution.State != "trusted" || solution.SourceTicketID != second.ID || solution.Report != nil || solution.RunID != task.RunID || s.Runs[task.RunID].State != "completed" {
				t.Fatalf("shared reviewed solution diverged: %+v", solution)
			}
		}
	}
}
