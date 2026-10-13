package esa

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// Seed through intake and human approval rather than assigning stored keywords
// or scores. This exercises case feature extraction as well as retrieval.
func seedRetrievalCase(t *testing.T, e *Engine, source Ticket, approve bool) Case {
	t.Helper()
	source.Closed = true
	source.Status = "Resolved"
	source.Resolution = Resolution{
		Reason: "fixed", Cause: "An empty partition assignment was dereferenced.",
		Solution:   "Check for an empty assignment before dereferencing it.",
		Validation: "An empty-assignment regression fails before the guard and passes after it.",
	}
	receipt := receive(t, e, "case-event-"+source.ID, source)
	if receipt.CaseID == "" {
		t.Fatal("resolved ticket did not create a case")
	}
	if approve {
		c, err := e.ReviewCase(receipt.CaseID, Review{Action: "approve", Reviewer: "fixture-engineer"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return e.Store.Snapshot().Cases[receipt.CaseID]
}

// These curated synthetic scenarios check operational feasibility and routing,
// not semantic accuracy on an independently labeled customer ticket archive.
func TestCaseRetrievalForwardedScenarios(t *testing.T) {
	scenarios := []struct {
		name                  string
		change                func(*Ticket)
		caseState             string
		candidate, applicable bool
		route, state          string
	}{
		{name: "matching failure", candidate: true, applicable: true, route: "case_reuse", state: "completed"},
		{name: "paraphrased symptom with same exception", change: func(q *Ticket) {
			q.Subject = "Empty ownership mapping breaks polling"
			q.Description = "NullPointerException is raised while handing off partitions."
		}, candidate: true, applicable: true, route: "case_reuse", state: "completed"},
		{name: "case insensitive metadata", change: func(q *Ticket) {
			q.Components = []string{"CONSUMER"}
			q.Environment = map[string]string{"details": "jdk 17"}
		}, candidate: true, applicable: true, route: "case_reuse", state: "completed"},
		{name: "structured error code", change: func(q *Ticket) {
			q.Subject, q.Description = "Polling stops", "Failure observed on ownership handoff."
			q.ErrorCodes = []string{"empty_assignment_npe"}
		}, candidate: true, applicable: true, route: "case_reuse", state: "completed"},
		{name: "exception in public comment", change: func(q *Ticket) {
			q.Subject, q.Description = "Polling stops", "Failure observed on ownership handoff."
			q.Comments = []Comment{{ID: "public-note", Body: "Attached stack trace contains NullPointerException."}}
		}, candidate: true, applicable: true, route: "case_reuse", state: "completed"},
		{name: "keyword-only match", change: func(q *Ticket) {
			q.Subject, q.Description = "Polling stops", "Consumer empty partition assignment stalls unexpectedly."
		}, candidate: true, applicable: true, route: "case_reuse", state: "completed"},
		{name: "similar vocabulary needs diagnosis", change: func(q *Ticket) {
			q.Subject, q.Description = "Polling stalls", "Consumer partition assignment stalls."
		}, candidate: true, route: "engineering_analysis", state: "queued"},
		{name: "different runtime", change: func(q *Ticket) {
			q.Environment = map[string]string{"details": "JDK 8"}
		}, candidate: true, route: "engineering_analysis", state: "queued"},
		{name: "different affected version", change: func(q *Ticket) {
			q.Versions = []string{"4.0.0"}
		}, candidate: true, route: "engineering_analysis", state: "queued"},
		{name: "different component", change: func(q *Ticket) {
			q.Components = []string{"broker"}
		}, candidate: true, route: "engineering_analysis", state: "queued"},
		{name: "other project", change: func(q *Ticket) {
			q.ProjectID = "2"
		}, route: "engineering_analysis", state: "queued"},
		{name: "unrelated symptoms", change: func(q *Ticket) {
			q.Subject, q.Description = "Heartbeat stalls", "Network probe blocks."
		}, route: "engineering_analysis", state: "queued"},
		{name: "missing environment", change: func(q *Ticket) {
			q.Environment = map[string]string{}
		}, candidate: true, route: "request_information", state: "awaiting_information"},
		{name: "unapproved case", caseState: "draft", route: "engineering_analysis", state: "queued"},
		{name: "rejected case", caseState: "rejected", route: "engineering_analysis", state: "queued"},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			e := testEngine(t)
			source := fixture(e, "resolved-source")
			source.ErrorCodes = []string{"EMPTY_ASSIGNMENT_NPE"}
			c := seedRetrievalCase(t, e, source, scenario.caseState != "draft")
			if scenario.caseState == "rejected" {
				if _, err := e.ReviewCase(c.ID, Review{Action: "reject", Reviewer: "fixture-engineer"}); err != nil {
					t.Fatal(err)
				}
			}
			query := fixture(e, "new-request")
			if scenario.change != nil {
				scenario.change(&query)
			}
			receipt := receive(t, e, "query-forward", query)
			run := e.Store.Snapshot().Runs[receipt.RunID]
			if run.Route != scenario.route || run.State != scenario.state {
				t.Fatalf("got %s/%s, want %s/%s", run.Route, run.State, scenario.route, scenario.state)
			}
			if (len(run.Matches) > 0) != scenario.candidate {
				t.Fatalf("candidate presence: got %v, want %v; matches=%+v", len(run.Matches) > 0, scenario.candidate, run.Matches)
			}
			for _, m := range run.Matches {
				if m.CaseID != c.ID || m.Applicable != scenario.applicable {
					t.Fatalf("unexpected match: %+v", m)
				}
				if !m.Applicable && m.Solution != "" {
					t.Fatal("unverified candidate exposed a directly reusable solution")
				}
			}
			if scenario.applicable {
				if !reflect.DeepEqual(run.Report.Recommendations, []string{c.Resolution.Solution}) {
					t.Fatal("reused report did not contain the approved repair", run.Report)
				}
				if len(run.Report.Evidence) != 1 || run.Report.Evidence[0].Source != "resolved_case" {
					t.Fatal("reused report lacks case provenance", run.Report)
				}
				called := false
				processed, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
					called = true
					return Report{}, nil
				}))
				if err != nil || called || processed || run.Report.Usage.Calls != 0 {
					t.Fatal("case reuse invoked an engineering provider", processed, called, err)
				}
			} else if len(run.Report.Recommendations) != 0 {
				t.Fatal("non-applicable case was recommended as a repair")
			}
		})
	}
}

func TestCaseRetrievalApplicableCaseSurvivesTopFiveLimit(t *testing.T) {
	e := testEngine(t)
	applicable := seedRetrievalCase(t, e, fixture(e, "approved-repair"), true)
	extra := "poll request fetch retry disconnect recovery network connection session heartbeat offset commit"
	for i, id := range []string{"near-a", "near-b", "near-c", "near-d", "near-e", "near-f"} {
		source := fixture(e, id)
		source.Description += " " + extra
		source.Versions = []string{"4.0.0"}
		if i%2 == 0 {
			source.Components = []string{"broker"}
		}
		seedRetrievalCase(t, e, source, true)
	}
	query := fixture(e, "request")
	query.Description += " " + extra
	receipt := receive(t, e, "forward", query)
	run := e.Store.Snapshot().Runs[receipt.RunID]
	if len(run.Matches) != 5 || run.Route != "case_reuse" || run.Matches[0].CaseID != applicable.ID {
		t.Fatal("applicable repair was displaced by similar but incompatible cases", run.Matches, run.Route)
	}
	for _, m := range run.Matches[1:] {
		if m.Applicable || m.Score <= run.Matches[0].Score {
			t.Fatal("fixture did not contain higher-scoring incompatible distractors", run.Matches)
		}
	}
	if !reflect.DeepEqual(run.Report.Recommendations, []string{applicable.Resolution.Solution}) {
		t.Fatal("distractors contaminated the recommended repair", run.Report.Recommendations)
	}
	// Repeated reads of unchanged history must produce stable ranking.
	s := e.Store.Snapshot()
	for i := 0; i < 10; i++ {
		matches := MatchCases(s.Tickets[query.ID], CasesAt(s.CaseHistory, e.Now()))
		if !reflect.DeepEqual(matches, run.Matches) {
			t.Fatal("retrieval order changed without input changes")
		}
	}
}

func TestCaseRetrievalRespectsApprovalAndWithdrawalTime(t *testing.T) {
	e := testEngine(t)
	created := e.Now()
	c := seedRetrievalCase(t, e, fixture(e, "source"), false)
	approved := created.Add(time.Hour)
	e.Now = func() time.Time { return approved }
	if _, err := e.ReviewCase(c.ID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	withdrawn := approved.Add(time.Hour)
	e.Now = func() time.Time { return withdrawn }
	if _, err := e.ReviewCase(c.ID, Review{Action: "reject", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	history := e.Store.Snapshot().CaseHistory
	for _, sample := range []struct {
		name  string
		at    time.Time
		reuse bool
	}{
		{"before case existed", created.Add(-time.Second), false},
		{"draft only", created, false},
		{"before approval", approved.Add(-time.Second), false},
		{"at approval", approved, true},
		{"before withdrawal", withdrawn.Add(-time.Second), true},
		{"at withdrawal", withdrawn, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			query := fixture(e, "request")
			query.ForwardedBy, query.ForwardedAt = "human", sample.at
			matches, route, _, _ := e.Decide(query, history, sample.at)
			if (route == "case_reuse") != sample.reuse {
				t.Fatalf("at %s got route %s", sample.at, route)
			}
			if sample.reuse && (len(matches) != 1 || matches[0].CaseVersion != 2) {
				t.Fatal("historical lookup used a future case revision", matches)
			}
		})
	}
}
