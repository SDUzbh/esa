package esa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testEngine(t testing.TB) *Engine {
	t.Helper()
	b, err := os.ReadFile("../../config/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err = json.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	e, err := NewEngine(store, config)
	if err != nil {
		t.Fatal(err)
	}
	e.IssueDirectory = t.TempDir()
	e.Now = func() time.Time { return time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC) }
	t.Cleanup(e.Store.verifyPublished)
	return e
}
func fixture(e *Engine, id string) Ticket {
	ticket := Ticket{ID: id, SourceKey: id, ProjectID: "1", Subject: "Consumer crashes with NullPointerException", Description: "Consumer crashes with NullPointerException during empty partition assignment.", Tracker: "Bug", Status: "New", Source: "test", Components: []string{"consumer"}, Versions: []string{"3.7.0"}, Environment: map[string]string{"details": "JDK 17"}, CreatedAt: e.Now(), UpdatedAt: e.Now()}
	return ticket
}
func receive(t *testing.T, e *Engine, id string, ticket Ticket) Receipt {
	t.Helper()
	// Forward only after the fixture's source edit becomes observable.
	if ticket.UpdatedAt.After(e.Now()) {
		at := ticket.UpdatedAt
		e.Now = func() time.Time { return at }
	}
	r, err := e.Receive(Event{ID: id, Origin: "test", ForwardedBy: "fixture-human", ForwardedAt: e.Now(), Ticket: ticket})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func resolved(t *testing.T, e *Engine) (Ticket, Receipt) {
	ticket := fixture(e, "closed")
	ticket.Closed = true
	ticket.Status = "Resolved"
	ticket.Resolution = Resolution{Reason: "fixed", Cause: "Empty assignment", Solution: "Upgrade after checking the assignment guard", Validation: "Verified empty assignment regression test"}
	return ticket, receive(t, e, "closed-event", ticket)
}
func TestIdempotencyAndChangedInput(t *testing.T) {
	e := testEngine(t)
	ticket := fixture(e, "1")
	r := receive(t, e, "event", ticket)
	d := receive(t, e, "event", ticket)
	if d.Action != "duplicate" || d.RunID != r.RunID {
		t.Fatal(d)
	}
	ticket.Assignee = "Engineer"
	ticket.UpdatedAt = ticket.UpdatedAt.Add(time.Second)
	cached := receive(t, e, "assignment", ticket)
	if cached.RunID != r.RunID {
		t.Fatal("metadata created a run")
	}
	ticket.Description += " Different reproduction"
	if _, err := e.Receive(Event{ID: "event", Origin: "test", ForwardedBy: "fixture-human", ForwardedAt: e.Now(), Ticket: ticket}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	stale := fixture(e, "1")
	stale.UpdatedAt = stale.UpdatedAt.Add(-time.Hour)
	if r := receive(t, e, "stale", stale); r.Action != "ignored_stale_event" {
		t.Fatal(r)
	}
	if r := receive(t, e, "new", ticket); r.RunID == cached.RunID {
		t.Fatal("changed input was cached")
	}
}
func TestCasesApprovalDoesNotRerouteAndWithdrawalRespectsPrivacy(t *testing.T) {
	e := testEngine(t)
	open := fixture(e, "open")
	initial := receive(t, e, "open-event", open)
	closed, receipt := resolved(t, e)
	if r := e.Store.Snapshot().Runs.Get(initial.RunID); r.Route != "engineering_analysis" {
		t.Fatal(r.Route)
	}
	if _, err := e.ReviewCase(receipt.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	r := s.Runs.Get(s.Latest.Get(open.ID))
	if r.ID != initial.RunID || r.Route != "engineering_analysis" || r.Report.Usage.Calls != 0 {
		t.Fatal(r)
	}
	closed.Assignee = "Other"
	closed.UpdatedAt = closed.UpdatedAt.Add(time.Second)
	receive(t, e, "closed-metadata", closed)
	if e.Store.Snapshot().Cases.Get(receipt.CaseID).State != "trusted" {
		t.Fatal("metadata revoked trust")
	}
	closed.Closed = false
	closed.UpdatedAt = closed.UpdatedAt.Add(time.Second)
	receive(t, e, "reopened", closed)
	if e.Store.Snapshot().Latest.Get(open.ID) != r.ID {
		t.Fatal("case withdrawal silently replaced an investigation")
	}
	if len(CasesAt(e.Store.Snapshot().CaseHistory, e.Now())) != 0 {
		t.Fatal("reopened case remains trusted")
	}
	closed.Closed = true
	closed.Private = true
	closed.UpdatedAt = closed.UpdatedAt.Add(time.Second)
	receive(t, e, "private", closed)
	if _, err := e.ReviewCase(receipt.CaseID, Review{Action: "approve", Reviewer: "engineer"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestAllResolvedCollection(t *testing.T) {
	e := testEngine(t)
	_, r := resolved(t, e)
	if _, err := e.ReviewCase(r.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"fixed", "duplicate", "rejected"} {
		ticket := fixture(e, reason)
		ticket.Subject = "How do I configure retention?"
		ticket.Description = "Documentation question"
		ticket.Closed = true
		ticket.Resolution.Reason = reason
		r := receive(t, e, reason, ticket)
		c := e.Store.Snapshot().Cases.Get(r.CaseID)
		want := map[string]CaseState{"fixed": CaseDraft, "duplicate": CaseDuplicate, "rejected": CaseArchived}[reason]
		if c.State != want {
			t.Fatal(c)
		}
	}
}

type analyzerFunc func(context.Context, AnalyzeRequest) (Report, error)

func (f analyzerFunc) Analyze(ctx context.Context, r AnalyzeRequest) (Report, error) {
	return f(ctx, r)
}
func TestAnalyzerGatesBudgetRecoveryAndStaleResults(t *testing.T) {
	t.Run("no provider", func(t *testing.T) {
		e := testEngine(t)
		r := receive(t, e, "a", fixture(e, "a"))
		e.ProcessOne(context.Background(), nil)
		run := e.Store.Snapshot().Runs.Get(r.RunID)
		if run.State != "awaiting_configuration" || run.Report.Usage.Calls != 0 {
			t.Fatal(run)
		}
	})
	t.Run("token warning does not reject diagnosis", func(t *testing.T) {
		e := testEngine(t)
		e.Config.Budget.MaxInputTokens = 1
		e.Config.Budget.MaxOutputTokens = 1
		e.Config.Budget.WarnTotalTokens = 1
		r := receive(t, e, "a", fixture(e, "a"))
		called := false
		e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
			called = true
			return Report{Issue: &IssueAssignment{Title: "Candidate", Reason: "Observed evidence"}, Summary: "Candidate", Evidence: []Evidence{{Source: "git", Reference: "old:10", Quote: "Observed"}}, Usage: Usage{Calls: 1, InputTokens: 100, OutputTokens: 200, TokensReported: true}}, nil
		}))
		run := e.Store.Snapshot().Runs.Get(r.RunID)
		if !called || run.Report.Usage.Calls != 1 || run.State != "awaiting_review" || len(run.Report.Alerts) != 3 {
			t.Fatal(run)
		}
	})
	t.Run("evidence and review", func(t *testing.T) {
		e := testEngine(t)
		r := receive(t, e, "a", fixture(e, "a"))
		e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
			if !in.ReadOnly {
				t.Fatal("not read-only")
			}
			return Report{Issue: &IssueAssignment{Title: "Candidate", Reason: "Observed check"}, Summary: "Candidate", Evidence: []Evidence{{Source: "git", Reference: "abc:src/file:10", Quote: "Observed check"}}}, nil
		}))
		run := e.Store.Snapshot().Runs.Get(r.RunID)
		if run.State != "awaiting_review" || run.Report.Usage.Calls != 1 || len(run.Report.Limitations) == 0 {
			t.Fatal(run)
		}
		task := e.Store.Snapshot().Investigations.Get(run.InvestigationID)
		if _, err := e.ReviewSolution(task.ID, SolutionReview{ExpectedSolutionID: task.SolutionID, Reviewer: "engineer", Action: "approve", Resolution: Resolution{Cause: "Reviewed candidate", Solution: "Apply the observed check", Validation: "Verify the reported scenario"}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("stale result", func(t *testing.T) {
		e := testEngine(t)
		ticket := fixture(e, "a")
		r := receive(t, e, "a", ticket)
		e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
			ticket.Description += " Changed"
			ticket.UpdatedAt = ticket.UpdatedAt.Add(time.Second)
			receive(t, e, "b", ticket)
			return Report{Issue: &IssueAssignment{Title: "Old", Reason: "Old evidence"}, Summary: "Old", Evidence: []Evidence{{Source: "git", Reference: "old", Quote: "Old"}}}, nil
		}))
		if e.Store.Snapshot().Runs.Get(r.RunID).State != "superseded" {
			t.Fatal("stale result published")
		}
	})
	t.Run("interrupted cannot be approved", func(t *testing.T) {
		e := testEngine(t)
		r := receive(t, e, "a", fixture(e, "a"))
		e.Store.Update(func(s *State) error {
			run := s.Runs.Get(r.RunID)
			run.State = "running"
			s.Runs.Put(run.ID, run)
			task := s.Investigations.Get(run.InvestigationID)
			task.State = "running"
			task.InvocationStarted = true
			task.LeaseUntil = e.Now().Add(-time.Second)
			s.Investigations.Put(task.ID, task)
			return nil
		})
		if err := e.Recover(); err != nil {
			t.Fatal(err)
		}
		run := e.Store.Snapshot().Runs.Get(r.RunID)
		if run.State != "failed" {
			t.Fatal("interrupted run remained active", run)
		}
		if _, err := e.ReviewSolution(run.InvestigationID, SolutionReview{Reviewer: "engineer", Action: "approve"}); err == nil {
			t.Fatal("interrupted investigation was approved")
		}
	})
}
func TestDurableStoreAndRollback(t *testing.T) {
	store := newTestStore(t)
	if err := store.Update(func(s *State) error { s.LastClaimedProject = "3"; return nil }); err != nil {
		t.Fatal(err)
	}
	paused := time.Date(2026, 10, 19, 15, 19, 0, 0, time.UTC)
	if err := store.Update(func(s *State) error { s.ProviderPausedUntil = paused; return nil }); err != nil {
		t.Fatal(err)
	}
	loaded := reopenTestStore(t, store)
	if loaded.Snapshot().LastClaimedProject != "3" || !loaded.Snapshot().ProviderPausedUntil.Equal(paused) {
		t.Fatal("committed Update lost across a restart")
	}
	if err := loaded.Update(func(s *State) error { s.LastClaimedProject = "4"; return ErrConflict }); !errors.Is(err, ErrConflict) || loaded.Snapshot().LastClaimedProject != "3" {
		t.Fatal("failed transaction committed")
	}
	// Snapshot is read-only and shared; Clone is the detached copy to edit.
	clone := loaded.Clone()
	clone.Tickets.Put("foreign", Ticket{})
	if loaded.Snapshot().Tickets.Len() != 0 {
		t.Fatal("clone aliases live state")
	}
}

// A ticket-scoped list visits only the scoped IDs; it must list exactly what
// an archive-wide walk would: scoped, existing, in-project tickets.
func TestTicketListHonorsTicketScope(t *testing.T) {
	e := testEngine(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		ticket := fixture(e, "redmine:"+id)
		if id == "d" {
			ticket.ProjectID = "2"
		}
		receive(t, e, id, ticket)
	}
	handler := (&API{Engine: e, Token: "test-token", OperatorToken: "operator-token"}).Handler()
	list := func(scopes ...string) []string {
		req := httptest.NewRequest("GET", "/v1/tickets?sort=id&direction=asc", nil)
		// Only the operator may list without a ticket scope.
		req.Header.Set("Authorization", "Bearer test-token")
		if scopes == nil {
			req.Header.Set("Authorization", "Bearer operator-token")
		}
		req.Header.Set("X-ESA-Project-IDs", "1")
		for _, scope := range scopes {
			req.Header.Add("X-ESA-Ticket-IDs", scope)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var body struct {
			Tickets []struct {
				Ticket Ticket `json:"ticket"`
			} `json:"tickets"`
			Total int `json:"total_count"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body.Total != len(body.Tickets) {
			t.Fatal(w.Code, w.Body.String())
		}
		ids := []string{}
		for _, row := range body.Tickets {
			ids = append(ids, row.Ticket.ID)
		}
		return ids
	}
	for _, tc := range []struct {
		scopes []string
		want   string
	}{
		{nil, "redmine:a,redmine:b,redmine:c"},
		{[]string{"redmine:c,redmine:missing,redmine:a,redmine:c", "redmine:d"}, "redmine:a,redmine:c"},
		{[]string{"none"}, ""},
	} {
		if got := strings.Join(list(tc.scopes...), ","); got != tc.want {
			t.Fatalf("scope %q listed %q, want %q", tc.scopes, got, tc.want)
		}
	}
}
func TestAPIAuthenticationScopeFilteringAndVisibility(t *testing.T) {
	e := testEngine(t)
	first := fixture(e, "redmine:1")
	receive(t, e, "1", first)
	second := fixture(e, "redmine:2")
	second.ProjectID = "2"
	receive(t, e, "2", second)
	handler := (&API{Engine: e, Token: "test-token"}).Handler()
	request := func(path, token, scope, issues string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		if scope != "" {
			req.Header.Set("X-ESA-Project-IDs", scope)
		}
		if issues != "" {
			req.Header.Set("X-ESA-Ticket-IDs", issues)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct {
		path, token, scope, issues string
		code                       int
	}{{"/v1/tickets", "", "", "", 401}, {"/v1/tickets", "test-token", "1", "", 401}, {"/v1/tickets", "Bearer test-token", "", "redmine:1", 403}, {"/v1/tickets", "Bearer test-token", "1", "", 403}, {"/v1/tickets/redmine:2", "Bearer test-token", "1", "redmine:1,redmine:2", 404}, {"/v1/tickets/redmine:1", "Bearer test-token", "1", "none", 404}, {"/v1/tickets?limit=-1", "Bearer test-token", "1", "redmine:1", 400}, {"/v1/tickets?sort=bogus", "Bearer test-token", "1", "redmine:1", 400}} {
		if w := request(tc.path, tc.token, tc.scope, tc.issues); w.Code != tc.code {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
	}
	w := request("/v1/tickets?sort=subject&direction=asc&limit=1", "Bearer test-token", "1", "redmine:1,redmine:2")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "redmine:2") {
		t.Fatal(w.Body.String())
	}
	_, r := resolved(t, e)
	e.ReviewCase(r.CaseID, Review{Action: "approve", Reviewer: "engineer"})
	w = request("/v1/tickets/redmine:1", "Bearer test-token", "1", "redmine:1")
	if strings.Contains(w.Body.String(), "Upgrade after checking") {
		t.Fatal("hidden case evidence leaked")
	}
}
func TestImportFormatsAndErrors(t *testing.T) {
	now := time.Now().UTC()
	data, err := os.ReadFile("../../examples/new-issues.csv")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseImport("tickets.csv", "1", data, now)
	if err != nil || len(p.Tickets) != 2 || p.Tickets[0].Components[0] != "consumer" {
		t.Fatal(p, err)
	}
	data = []byte(`{"issue":{"key":"KAFKA-1","fields":{"summary":"Example","issuetype":{"name":"Bug"},"status":{"name":"Open"},"created":"2012-10-10T16:39:01.332+0000","updated":"2012-10-10T16:39:01.332+0000","components":[{"name":"consumer"}],"versions":[{"name":"3.7.0"}]}},"comments":[{"id":"1","body":"Evidence","created":"2012-10-10T16:39:01.332+0000"}]}`)
	p, err = ParseImport("jira.json", "1", data, now)
	if err != nil || len(p.Tickets) != 1 || len(p.Tickets[0].Comments) != 1 || p.Tickets[0].ProjectID != "1" {
		t.Fatal(p, err)
	}
	if _, err = ParseImport("bad.json", "1", []byte("{"), now); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	p, err = ParseImport("bad.csv", "1", []byte("Issue key,Summary,Updated\na,Subject,not-a-date\n"), now)
	if err != nil || len(p.Problems) != 1 || len(p.Tickets) != 0 {
		t.Fatal(p, err)
	}
	p, err = ParseImport("comments.csv", "1", []byte("Issue key,Summary,Comment,Comment\na,Subject,First,Second\n"), now)
	if err != nil || len(p.Tickets[0].Comments) != 2 {
		t.Fatal(p, err)
	}
}
