package esa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sharedInvestigationEngine(t *testing.T) *Engine {
	t.Helper()
	e := testEngine(t)
	e.Config.Worker.LeaseSeconds = 3
	return e
}
func diagnosticReport() Report {
	ev := Evidence{Source: "git", Reference: "registered:Example.java:1", Quote: "assignment.size()"}
	return Report{Issue: &IssueAssignment{Title: "Empty assignment dereference", Reason: "Inspected assignment access"}, Summary: "Empty assignment may be dereferenced", Evidence: []Evidence{ev}, Findings: []Finding{{Commit: "registered", File: "Example.java", StartLine: 1, EndLine: 1, Problem: "Empty assignment may be dereferenced", Solution: "Check assignment presence", Evidence: []Evidence{ev}}}, Recommendations: []string{"Check assignment presence"}, Usage: Usage{Calls: 1, InputTokens: 20, OutputTokens: 10, TokensReported: true}}
}
func TestSharedInvestigationsConcurrentReleaseIncidentSharesOneDiagnosis(t *testing.T) {
	e := sharedInvestigationEngine(t)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	ids := make(chan string, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ticket := fixture(e, fmt.Sprintf("ticket-%03d", i))
			r, err := e.Receive(Event{ID: ticket.ID, Origin: "test", ForwardedBy: "human", ForwardedAt: e.Now(), Ticket: ticket})
			if err != nil {
				errs <- err
			} else {
				ids <- r.RunID
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatal(err)
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	s := e.Store.Snapshot()
	if len(unique) != 1 || len(s.Investigations) != 1 || len(s.Issues) != 0 || len(s.Snapshots) != 100 || len(s.Memberships) != 100 {
		t.Fatalf("duplicate investigations: runs=%d tasks=%d issues=%d snapshots=%d links=%d", len(unique), len(s.Investigations), len(s.Issues), len(s.Snapshots), len(s.Memberships))
	}
	var calls atomic.Int64
	provider := analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		calls.Add(1)
		if in.Contract == nil || !in.Contract.ReadOnly || in.Contract.IssueID != "" || len(inputIssueRecords(t, e, in)) != 0 {
			return Report{}, errors.New("missing task contract or known issue")
		}
		return diagnosticReport(), nil
	})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.ProcessOne(context.Background(), provider); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("repeated engineering invocations", calls.Load())
	}
	s = e.Store.Snapshot()
	if len(s.Issues) != 1 {
		t.Fatal("analysis did not create one shared issue", len(s.Issues))
	}
	task := sortedInvestigations(s)[0]
	proposal := s.Solutions[task.SolutionID]
	if task.State != "agent_proposed" || proposal.State != "proposed" || proposal.Origin != "agent" {
		t.Fatal("proposal incorrectly trusted", task, proposal)
	}
	frozen := s.Snapshots[task.Contract.SnapshotID]
	if frozen.Hash != task.Contract.InputHash {
		t.Fatal("snapshot changed")
	}
	receive(t, e, "later", fixture(e, "later"))
	if len(e.Store.Snapshot().Investigations) != 1 {
		t.Fatal("pending report was not reused")
	}
	_, err := e.ProcessOne(context.Background(), provider)
	if err != nil || calls.Load() != 1 {
		t.Fatal("repeat spend on proposed result", err, calls.Load())
	}
	review := SolutionReview{ExpectedSolutionID: proposal.ID, Action: "approve", Reviewer: "engineer", Resolution: Resolution{Cause: "Empty assignment dereference", Solution: "Check presence", Validation: "Verified regression"}}
	approved, err := e.ReviewSolution(task.ID, review)
	if err != nil {
		t.Fatal(err)
	}
	if approved.State != "trusted" || approved.Origin != "engineer" || approved.Revision != 2 || approved.Supersedes != proposal.ID {
		t.Fatal(approved)
	}
	if e.Store.Snapshot().Solutions[proposal.ID].State != "proposed" {
		t.Fatal("review erased proposal history")
	}
	if _, err = e.ReviewSolution(task.ID, review); !errors.Is(err, ErrConflict) {
		t.Fatal("stale review overwrote a newer revision", err)
	}
}

func TestSharedInvestigationsNoveltyAndContextAreNotMerged(t *testing.T) {
	e := sharedInvestigationEngine(t)
	initial := receive(t, e, "one", fixture(e, "one"))
	for i, change := range []func(*Ticket){func(t *Ticket) { t.ProjectID = "2" }, func(t *Ticket) { t.Versions = []string{"4.0"} }, func(t *Ticket) { t.Environment = map[string]string{"details": "JDK 8"} }, func(t *Ticket) { t.Description = "Different NullPointerException while validating a configuration" }} {
		ticket := fixture(e, fmt.Sprintf("changed-%d", i))
		change(&ticket)
		r := receive(t, e, ticket.ID, ticket)
		if r.RunID == initial.RunID {
			t.Fatal("incompatible context merged")
		}
	}
	if len(e.Store.Snapshot().Investigations) != 5 {
		t.Fatal("missing independent investigations")
	}
}
func TestSharedInvestigationsVisibilityAndFrozenSnapshots(t *testing.T) {
	e := sharedInvestigationEngine(t)
	receive(t, e, "owner-event", fixture(e, "owner"))
	r := receive(t, e, "member-event", fixture(e, "member"))
	e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil }))
	w := classifierRequest(e, "GET", "/v1/tickets/member", "", "1", "member")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"source_ticket_id":"owner"`) || strings.Contains(w.Body.String(), `"quote":"assignment.size()"`) {
		t.Fatal("shared owner evidence leaked", w.Code, w.Body)
	}
	w = classifierRequest(e, "GET", "/v1/engineering-issues", "", "1", "member")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"visible_ticket_count":1`) || strings.Contains(w.Body.String(), `"owner"`) {
		t.Fatal("issue members crossed visibility", w.Body)
	}
	s := e.Store.Snapshot()
	task := s.Investigations[s.Runs[r.RunID].InvestigationID]
	snap := s.Snapshots[task.Contract.SnapshotID]
	w = classifierRequest(e, "GET", "/v1/snapshots/"+snap.ID, "", "1", "member")
	if w.Code != 404 {
		t.Fatal("private owner snapshot exposed", w.Code)
	}
	changed := fixture(e, "owner")
	changed.Private = true
	receive(t, e, "owner-private", changed)
	if e.Store.Snapshot().Snapshots[snap.ID].Hash != snap.Hash {
		t.Fatal("mutable original snapshot")
	}
	w = classifierRequest(e, "GET", "/v1/investigations/"+task.ID, "", "1", "member")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"solution":`) {
		t.Fatal("private-source solution leaked", w.Code, w.Body)
	}
}
func TestSharedInvestigationsLeaseFencesStaleResults(t *testing.T) {
	e := sharedInvestigationEngine(t)
	var clock atomic.Int64
	clock.Store(e.Now().UnixNano())
	e.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	receive(t, e, "forward", fixture(e, "ticket"))
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
			close(started)
			<-release
			return diagnosticReport(), nil
		}))
		finished <- err
	}()
	<-started
	clock.Add(int64(4 * time.Second))
	if err := e.Recover(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; !errors.Is(err, ErrConflict) {
		t.Fatal("stale worker published a report", err)
	}
	task := sortedInvestigations(e.Store.Snapshot())[0]
	if task.State != "failed" || task.Epoch != 2 || len(e.Store.Snapshot().Solutions) != 0 {
		t.Fatal("lease failure was not fenced", task)
	}
	calls := 0
	e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { calls++; return diagnosticReport(), nil }))
	if calls != 0 {
		t.Fatal("uncertain spend retried automatically")
	}
}
func TestSharedInvestigationsReplayClockAndFutureCaseIsolation(t *testing.T) {
	e := sharedInvestigationEngine(t)
	base := e.Now()
	e.Config.Replay.Enabled = true
	e.Config.Replay.Projects = []string{"1"}
	e.clock.Store(base.UnixNano())
	e.Now = func() time.Time { return time.Unix(0, e.clock.Load()).UTC() }
	source, r := resolved(t, e)
	if _, err := e.ReviewCase(r.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	query := fixture(e, "historical")
	query.CreatedAt = base.Add(-time.Hour)
	query.UpdatedAt = query.CreatedAt
	receipt, err := e.Receive(Event{ID: "historical", Origin: "test", ForwardedBy: "human", ForwardedAt: query.CreatedAt, Ticket: query})
	if err != nil {
		t.Fatal(err)
	}
	_ = receipt
	_ = source
	b, _ := json.Marshal(map[string]time.Time{"time": base.Add(time.Hour)})
	w := classifierRequest(e, "POST", "/v1/replay/clock", string(b), "1", "historical")
	if w.Code != 200 || !e.Now().Equal(base.Add(time.Hour)) {
		t.Fatal(w.Code, w.Body)
	}
	b, _ = json.Marshal(map[string]time.Time{"time": base})
	w = classifierRequest(e, "POST", "/v1/replay/clock", string(b), "1", "historical")
	if w.Code != 400 {
		t.Fatal("clock rewound", w.Code)
	}
	req := httptest.NewRequest("POST", "/v1/replay/clock", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-ESA-Project-IDs", "2")
	out := httptest.NewRecorder()
	(&API{Engine: e, Token: "token"}).Handler().ServeHTTP(out, req)
	if out.Code != 403 {
		t.Fatal("clock scope bypassed", out.Code)
	}
}
