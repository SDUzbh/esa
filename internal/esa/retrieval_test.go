package esa

import (
	"context"
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

// These synthetic scenarios verify admission and context delivery, not model accuracy.
func TestCaseContextNeverBypassesAgent(t *testing.T) {
	scenarios := []struct {
		name    string
		change  func(*Ticket)
		state   string
		context bool
	}{
		{name: "matching failure", context: true},
		{name: "different runtime", change: func(q *Ticket) { q.Environment = map[string]string{"details": "JDK 8"} }, context: true},
		{name: "different version", change: func(q *Ticket) { q.Versions = []string{"4.0.0"} }, context: true},
		{name: "different component", change: func(q *Ticket) { q.Components = []string{"broker"} }, context: true},
		{name: "unrelated symptoms", change: func(q *Ticket) { q.Subject = "Heartbeat stalls"; q.Description = "Network probe blocks" }, context: true},
		{name: "missing metadata", change: func(q *Ticket) { q.Components = nil; q.Versions = nil; q.Environment = nil }, context: true},
		{name: "other project", change: func(q *Ticket) { q.ProjectID = "2" }},
		{name: "draft case", state: "draft"},
		{name: "rejected case", state: "rejected"},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			e := testEngine(t)
			c := seedRetrievalCase(t, e, fixture(e, "source"), tc.state != "draft")
			if tc.state == "rejected" {
				if _, err := e.ReviewCase(c.ID, Review{Action: "reject", Reviewer: "engineer"}); err != nil {
					t.Fatal(err)
				}
			}
			query := fixture(e, "query")
			if tc.change != nil {
				tc.change(&query)
			}
			receipt := receive(t, e, "forward", query)
			run := e.Store.Snapshot().Runs[receipt.RunID]
			if run.Route != "engineering_analysis" || run.State != "queued" || len(run.Report.Recommendations) != 0 {
				t.Fatal("host made a diagnosis", run)
			}
			if len(run.Matches) != 0 {
				t.Fatal("intake still preselected cases", run.Matches)
			}
			for _, m := range run.Matches {
				if m.Applicable || m.Solution != "" {
					t.Fatal("host asserted applicability", m)
				}
			}
			calls := 0
			processed, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
				calls++
				if (len(inputIssueRecords(t, e, in)) > 0) != tc.context {
					t.Fatal("case context lost", inputIssueRecords(t, e, in))
				}
				if tc.context && (inputIssueRecords(t, e, in)[0].CaseID != c.ID || inputIssueRecords(t, e, in)[0].Recommendations[0] != c.Resolution.Solution) {
					t.Fatal("case details lost", inputIssueRecords(t, e, in))
				}
				return Report{Summary: "More information required", Limitations: []string{"Provide reproduction steps"}}, nil
			}))
			if err != nil || !processed || calls != 1 {
				t.Fatal("Agent was bypassed", processed, calls, err)
			}
			state := e.Store.Snapshot()
			if state.Runs[receipt.RunID].State != "awaiting_information" || len(state.Issues) != 0 {
				t.Fatal("unsupported answer created an issue", state.Runs[receipt.RunID])
			}
		})
	}
}

func TestIssueDirectoryDoesNotTruncateArchive(t *testing.T) {
	e := testEngine(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		seedRetrievalCase(t, e, fixture(e, id), true)
	}
	receive(t, e, "forward", fixture(e, "query"))
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		records := inputIssueRecords(t, e, in)
		if len(records) != 7 {
			t.Fatal("archive was truncated", len(records))
		}
		return Report{Summary: "Need more context", Limitations: []string{"Synthetic test"}}, nil
	})); err != nil {
		t.Fatal(err)
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
			matches := CasesAt(history, sample.at)
			if (len(matches) > 0) != sample.reuse {
				t.Fatalf("at %s got context %+v", sample.at, matches)
			}
			if sample.reuse && (len(matches) != 1 || matches[0].Version != 2) {
				t.Fatal("historical lookup used a future case revision", matches)
			}
		})
	}
}
