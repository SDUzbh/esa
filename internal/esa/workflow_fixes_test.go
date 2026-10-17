package esa

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func analyzedTicket(t *testing.T, e *Engine, id string) (Ticket, Run) {
	t.Helper()
	ticket := fixture(e, id)
	ticket.Description += " " + id
	r := receive(t, e, id, ticket)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	return ticket, e.Store.Snapshot().Runs.Get(r.RunID)
}
func TestMembershipChangesNeverScheduleWork(t *testing.T) {
	e := testEngine(t)
	ticket, run := analyzedTicket(t, e, "source")
	before := e.Store.Snapshot()
	link, _ := activeMembership(before, ticket.ID)
	review := MembershipReview{ExpectedSnapshotID: before.LatestSnapshot.Get(ticket.ID), ExpectedMembershipID: link.ID, IssueID: run.IssueID, Reviewer: "engineer", Reason: "Confirm grouping"}
	for i := 0; i < 2; i++ {
		if _, err := e.ReviewMembership(ticket.ID, review); err != nil {
			t.Fatal(err)
		}
	}
	after := e.Store.Snapshot()
	if after.Investigations.Len() != 1 || after.Runs.Len() != 1 || after.Events.Len() != before.Events.Len() || after.Snapshots.Len() != before.Snapshots.Len() || hash(after.Runs.Get(run.ID)) != hash(before.Runs.Get(run.ID)) {
		t.Fatal("pure membership confirmation changed analysis or forwarding")
	}
	review.IssueID = ""
	split, err := e.ReviewMembership(ticket.ID, review)
	if err != nil {
		t.Fatal(err)
	}
	after = e.Store.Snapshot()
	current, _ := activeMembership(after, ticket.ID)
	if split.RunID != run.ID || current.IssueID == run.IssueID || after.Investigations.Len() != 1 {
		t.Fatal("split queued work or retained issue")
	}
	if _, err = e.ReviewMembership(ticket.ID, review); err == nil {
		t.Fatal("stale split was repeated")
	}
	if did, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		t.Fatal("membership invoked a provider")
		return Report{}, nil
	})); err != nil || did {
		t.Fatal(did, err)
	}
	row := rowFor(after, ticket)
	if row.EngineeringIssue.ID != current.IssueID || row.Investigation != nil || row.Solution != nil || row.LatestRun.State != "membership_reviewed" {
		t.Fatal("old result was applied to the new membership", row)
	}
	// An explicit later request can analyze the target without undoing the review.
	retry, err := e.AnalyzeTicket(ticket.ID, true, "engineer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().Runs.Get(retry.RunID).IssueID != current.IssueID {
		t.Fatal("explicit analysis overrode reviewed membership")
	}
}
func TestCanonicalReviewAutoCollectsAndWithdrawsCase(t *testing.T) {
	e := testEngine(t)
	ticket, run := analyzedTicket(t, e, "source")
	state := e.Store.Snapshot()
	task := state.Investigations.Get(run.InvestigationID)
	resolution := Resolution{Reason: "fixed", Cause: "Confirmed cause", Solution: "Confirmed guard", Validation: "Confirmed regression"}
	approved, err := e.ReviewSolution(task.ID, SolutionReview{ExpectedSolutionID: task.SolutionID, Action: "approve", Reviewer: "engineer", Resolution: resolution})
	if err != nil {
		t.Fatal(err)
	}
	at := e.Now().Add(time.Minute)
	e.Now = func() time.Time { return at }
	ticket.Closed = true
	ticket.Resolution = resolution
	ticket.UpdatedAt = at
	closed := receive(t, e, "closed", ticket)
	state = e.Store.Snapshot()
	c := state.Cases.Get(closed.CaseID)
	if c.State != "trusted" || c.SolutionID != approved.ID || state.Solutions.Len() != 2 {
		t.Fatal("closing a reviewed issue required a second approval", c)
	}
	if _, err = e.ReviewCase(c.ID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().Solutions.Len() != 2 {
		t.Fatal("repeated approval duplicated knowledge")
	}
	withdrawn, err := e.ReviewSolution(task.ID, SolutionReview{ExpectedSolutionID: approved.ID, Action: "withdraw", Reviewer: "engineer"})
	if err != nil {
		t.Fatal(err)
	}
	state = e.Store.Snapshot()
	if state.Cases.Get(c.ID).State != "withdrawn" || state.Cases.Get(c.ID).SolutionID != withdrawn.ID {
		t.Fatal("case retained independent trust")
	}
	query := fixture(e, "query")
	records := issueRecords(state, Investigation{Contract: TaskContract{ObservationCutoff: at}}, query)
	if len(records) != 0 {
		t.Fatal("withdrawn knowledge remained retrievable", records)
	}
}
func TestCaseReviewPublishesOneSharedCanonicalRevision(t *testing.T) {
	e := testEngine(t)
	first, a := analyzedTicket(t, e, "one")
	second := fixture(e, "two")
	second.Description += " related independent report"
	r := receive(t, e, "two", second)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		report := diagnosticReport()
		report.Issue.ExistingID = a.IssueID
		return report, nil
	})); err != nil {
		t.Fatal(err)
	}
	second.Closed = true
	second.Resolution = Resolution{Cause: "Reviewed cause", Solution: "Reviewed fix", Validation: "Reviewed regression"}
	closed := receive(t, e, "close-two", second)
	before := e.Store.Snapshot().Solutions.Len()
	c, err := e.ReviewCase(closed.CaseID, Review{Action: "approve", Reviewer: "engineer"})
	if err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	ta, tb := state.Investigations.Get(a.InvestigationID), state.Investigations.Get(state.Runs.Get(r.RunID).InvestigationID)
	if state.Solutions.Len() != before+1 || ta.SolutionID != tb.SolutionID || c.SolutionID != ta.SolutionID {
		t.Fatal("case review duplicated canonical knowledge")
	}
	records := issueRecords(state, Investigation{Contract: TaskContract{ObservationCutoff: e.Now()}}, fixture(e, "query"))
	if len(records) != 1 || records[0].SolutionID != ta.SolutionID || records[0].Summary != second.Resolution.Cause {
		t.Fatal("directory did not use canonical shared knowledge", records)
	}
	for _, task := range []Investigation{ta, tb} {
		response := classifierRequest(e, "GET", "/v1/investigations/"+task.ID+"/solutions", "", "1", "one,two")
		if response.Code != 200 || !strings.Contains(response.Body.String(), ta.SolutionID) {
			t.Fatal("canonical review missing from shared history", response.Body)
		}
	}
	if _, err = e.ReviewSolution(ta.ID, SolutionReview{ExpectedSolutionID: ta.SolutionID, Action: "reject", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().Cases.Get(c.ID).State != "rejected" {
		t.Fatal("solution rejection did not reach linked case")
	}
	_ = first
}
func TestReuseCreatesApplicationWithoutNewKnowledge(t *testing.T) {
	e := testEngine(t)
	_, original := analyzedTicket(t, e, "source")
	query := fixture(e, "query")
	query.Description += " query"
	r := receive(t, e, "query", query)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) { return reuseFirstIssue(t, e, in), nil })); err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	task := state.Investigations.Get(state.Runs.Get(r.RunID).InvestigationID)
	if state.Solutions.Len() != 1 || task.SolutionID != "" || task.Application == nil || task.Application.SolutionID != state.Investigations.Get(original.InvestigationID).SolutionID {
		t.Fatal("reuse created another knowledge entity", task)
	}
	sourceID := task.Application.SolutionID
	review := SolutionReview{ExpectedSolutionID: sourceID, ExpectedApplicationRevision: 1, Action: "approve", Reviewer: "engineer"}
	if _, err := e.ReviewSolution(task.ID, review); err != nil {
		t.Fatal(err)
	}
	state = e.Store.Snapshot()
	if state.Solutions.Len() != 1 || state.Solutions.Get(sourceID).State != "proposed" || state.Investigations.Get(task.ID).Application.State != "accepted" {
		t.Fatal("applicability approval promoted source knowledge")
	}
	if _, err := e.ReviewSolution(task.ID, review); err == nil {
		t.Fatal("stale application review accepted")
	}
	owner := state.Investigations.Get(original.InvestigationID)
	if _, err := e.ReviewSolution(owner.ID, SolutionReview{ExpectedSolutionID: sourceID, Action: "withdraw", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	if e.Store.Snapshot().Investigations.Get(task.ID).Application.State != "withdrawn" {
		t.Fatal("source withdrawal did not invalidate application")
	}
	response := classifierRequest(e, "GET", "/v1/tickets/query", "", "1", "source,query")
	var current struct {
		Ticket TicketRow `json:"ticket"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(current.Ticket.LatestRun.Report.Recommendations) != 0 {
		t.Fatal("withdrawn application still showed current advice", response.Body)
	}
}
func TestCanonicalTitleAndRepresentativeStaySeparate(t *testing.T) {
	e := testEngine(t)
	ticket, run := analyzedTicket(t, e, "z-source")
	follower := ticket
	follower.ID = "a-follower"
	follower.SourceKey = follower.ID
	follower.Subject = ticket.Subject // exact input is intentionally shared
	receive(t, e, "follower", follower)
	for _, scope := range []string{"z-source,a-follower", "a-follower"} {
		w := classifierRequest(e, "GET", "/v1/engineering-issues", "", "1", scope)
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		var result struct {
			Issues []struct {
				Issue          EngineeringIssue `json:"issue"`
				Representative Ticket           `json:"representative_ticket"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Issues) != 1 {
			t.Fatal(w.Body)
		}
		row := result.Issues[0]
		if row.Representative.Subject != ticket.Subject {
			t.Fatal("representative missing")
		}
		if scope == "a-follower" {
			if row.Issue.Title != "" || !row.Issue.TitleRestricted {
				t.Fatal("source title leaked")
			}
		} else if row.Issue.Title != e.Store.Snapshot().Issues.Get(run.IssueID).Title {
			t.Fatal("canonical title was replaced")
		}
	}
	state := e.Store.Clone()
	private := fixture(e, "private-title-source")
	private.Private = true
	state.Tickets.Put(private.ID, private)
	issue := state.Issues.Get(run.IssueID)
	issue.TitleSourceTicketID = private.ID
	state.Issues.Put(issue.ID, issue)
	records := issueRecords(state, Investigation{Contract: TaskContract{ObservationCutoff: e.Now()}}, fixture(e, "query"))
	if len(records) != 1 || records[0].Title != "" {
		t.Fatal("directory exposed a private canonical title", records)
	}
}
func TestRetiredFieldsRemainInertAndRoundTrip(t *testing.T) {
	var oldSolution SolutionRevision
	legacy := `{"id":"solution_old","report":{"summary":"historical inline evidence","agent_classification":{"category":"old"}}}`
	if err := json.Unmarshal([]byte(legacy), &oldSolution); err != nil {
		t.Fatal(err)
	}
	archived, err := json.Marshal(oldSolution)
	if err != nil || !strings.Contains(string(archived), `"agent_classification"`) || !strings.Contains(string(archived), `"report"`) {
		t.Fatal("historical inline solution evidence lost", err)
	}
	raw := `{"id":"old","route":"case_reuse","matches":[{"solution":"retired private advice"}],"case_generation":3,"classification":{"category":"old"},"report":{"summary":"Historical report","agent_classification":{"category":"old"}}}`
	var run Run
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"matches"`, `"case_generation"`, `"classification"`, `"agent_classification"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatal("stored historical field lost", field)
		}
	}
	view := visibleRun(httptest.NewRequest("GET", "/", nil), State{}, run)
	if view.Report.Summary == run.Report.Summary || len(view.StoredFields) != 0 {
		t.Fatal("historical cross-ticket evidence without provenance was exposed")
	}
	var request Ticket
	if err = json.Unmarshal([]byte(`{"id":"new","labels":["old"],"category_override":"old"}`), &request); err != nil {
		t.Fatal(err)
	}
	request.ProjectID = "1"
	request.Subject = "A forwarded ticket"
	e := testEngine(t)
	request.CreatedAt = e.Now()
	request.UpdatedAt = e.Now()
	receipt := receive(t, e, "new", request)
	active := e.Store.Snapshot().Runs.Get(receipt.RunID)
	if active.Route != "engineering_analysis" || len(active.Input.StoredFields) != 0 || len(active.StoredFields) != 0 {
		t.Fatal("retired fields entered active workflow")
	}
}
func TestIssueViewAndCompiledIndexAreReused(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []IssueRecord{{ID: "issue", IssueID: "issue", ProjectID: "1", Kind: "agent_analyzed", ScopeHash: "scope", Title: "Empty assignment", Summary: "NullPointerException", Recommendations: []string{"Guard"}, AvailableAt: at}}
	first, digest, err := ExportIssueDirectory(root, "1", at, records)
	if err != nil {
		t.Fatal(err)
	}
	second, again, err := ExportIssueDirectory(root, "1", at.Add(time.Hour), records)
	if err != nil || first != second || digest != again {
		t.Fatal("unchanged knowledge created another view", err)
	}
	directory, err := OpenIssueDirectory(root, first, digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = directory.Search("assignment", "", 0, 10); err != nil {
		t.Fatal(err)
	}
	index := directory.searchIndexes[""]
	if _, _, err = directory.Search("NullPointerException", "", 0, 10); err != nil || directory.searchIndexes[""] != index {
		t.Fatal("index was rebuilt", err)
	}
	records[0].Recommendations = []string{"Updated guard"}
	changed, _, err := ExportIssueDirectory(root, "1", at.Add(time.Hour), records)
	if err != nil || changed == first {
		t.Fatal("changed knowledge reused stale view", err)
	}
}
