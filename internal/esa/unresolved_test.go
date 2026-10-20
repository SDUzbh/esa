package esa

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// unresolvedProvider routes every ticket to analysis, starting from the first
// unresolved record when choose returns one, and abstains in engineering.
func unresolvedProvider(t *testing.T, e *Engine, choose func([]IssueRecord) *RoutingResult) (PipelineAnalyzer, *[]string) {
	var mu sync.Mutex
	routed := map[string]RoutingResult{}
	engineered := []string{}
	return pipelineFunc(func(_ context.Context, in AnalyzeRequest, stage string) (Report, error) {
		mu.Lock()
		defer mu.Unlock()
		if stage == "route" {
			r := intakeReport("analyze")
			if decision := choose(inputIssueRecords(t, e, in)); decision != nil {
				r.Routing.Action, r.Routing.RecordPath, r.Routing.RecordHash = decision.Action, decision.RecordPath, decision.RecordHash
			}
			routed[in.RunID] = *r.Routing
			return r, nil
		}
		engineered = append(engineered, in.Ticket.ID)
		decision := routed[in.RunID]
		return Report{
			Summary:     "Reported deadlock frames are not in the frozen release",
			Limitations: []string{"onCompleteJoin has no groupManager.store call; the dump came from newer code"},
			Routing:     &decision,
			Usage:       Usage{Calls: 2, TokensReported: true},
		}, nil
	}), &engineered
}

func unresolvedRecords(records []IssueRecord) []IssueRecord {
	var rows []IssueRecord
	for _, r := range records {
		if r.Kind == "unresolved" {
			rows = append(rows, r)
		}
	}
	return rows
}

func TestUnresolvedInvestigationGuidesDuplicateAnalysis(t *testing.T) {
	e := testEngine(t)
	var seen []IssueRecord
	provider, engineered := unresolvedProvider(t, e, func(records []IssueRecord) *RoutingResult {
		seen = unresolvedRecords(records)
		if len(seen) == 0 {
			return nil
		}
		return &RoutingResult{Action: "analyze", RecordPath: recordPath(seen[0]), RecordHash: issueRecordHash(seen[0])}
	})
	original := receive(t, e, "original", fixture(e, "original"))
	if _, err := e.ProcessOne(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if state := e.Store.Snapshot().Investigations.Get(e.Store.Snapshot().Runs.Get(original.RunID).InvestigationID).State; state != InvestigationAwaitingInformation {
		t.Fatal("the abstaining original did not await information", state)
	}
	duplicate := fixture(e, "duplicate")
	duplicate.Versions = []string{"4.0.0"}
	duplicate.Description += " seen again on a newer release"
	second := receive(t, e, "duplicate", duplicate)
	if _, err := e.ProcessOne(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].SourceTicketID != "original" || seen[0].IssueID != "" || seen[0].SolutionID != "" ||
		len(seen[0].Limitations) != 1 || seen[0].Summary == "" || seen[0].Versions[0] == "4.0.0" {
		t.Fatal("the duplicate's directory did not hold the original's unresolved record", seen)
	}
	run := e.Store.Snapshot().Runs.Get(second.RunID)
	if run.Error != "" || run.Report.Routing.RecordPath != recordPath(seen[0]) || len(*engineered) != 2 {
		t.Fatal("analysis did not start from the unresolved record", run.Error, run.Report.Routing, *engineered)
	}
	// Neither unresolved record is a solution: nothing was published.
	if s := e.Store.Snapshot(); s.Issues.Len() != 0 || s.Solutions.Len() != 0 {
		t.Fatal("an unresolved record created an Issue or solution")
	}
}

func TestUnresolvedRecordIsNeverReusedOrJoined(t *testing.T) {
	for action, refusal := range map[string]string{
		"reuse":         "not a reusable solution",
		"join":          "eligible investigating record",
		"analyze-other": "cannot assign or publish a solution",
	} {
		t.Run(action, func(t *testing.T) {
			e := testEngine(t)
			provider, engineered := unresolvedProvider(t, e, func(records []IssueRecord) *RoutingResult {
				rows := unresolvedRecords(records)
				if len(rows) == 0 {
					return nil
				}
				if action == "analyze-other" {
					// A record the directory does not hold.
					return &RoutingResult{Action: "analyze", RecordPath: recordPath(rows[0]), RecordHash: hash("other")}
				}
				return &RoutingResult{Action: action, RecordPath: recordPath(rows[0]), RecordHash: issueRecordHash(rows[0])}
			})
			receive(t, e, "original", fixture(e, "original"))
			if _, err := e.ProcessOne(context.Background(), provider); err != nil {
				t.Fatal(err)
			}
			duplicate := fixture(e, "duplicate")
			duplicate.Description += " seen again"
			second := receive(t, e, "duplicate", duplicate)
			if _, err := e.ProcessOne(context.Background(), provider); err != nil {
				t.Fatal(err)
			}
			s := e.Store.Snapshot()
			run := s.Runs.Get(second.RunID)
			if task := s.Investigations.Get(run.InvestigationID); task.State != InvestigationFailed || !strings.Contains(run.Error, refusal) || len(*engineered) != 1 {
				t.Fatal("an unresolved record was accepted as", action, task.State, run.Error, *engineered)
			}
		})
	}
}

func TestOnlyEngineeringAbstentionsBecomeUnresolvedRecords(t *testing.T) {
	e := testEngine(t)
	// Routing asked for information before any engineering: nothing was checked.
	receive(t, e, "unrouted", fixture(e, "unrouted"))
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		return intakeReport("request_information"), nil
	})); err != nil {
		t.Fatal(err)
	}
	query := fixture(e, "query")
	query.Description += " new report"
	receive(t, e, "query", query)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		if rows := unresolvedRecords(inputIssueRecords(t, e, in)); len(rows) != 0 {
			t.Error("a routing clarification was exported as an unresolved investigation", rows)
		}
		return Report{Summary: "Fixture clarification", Limitations: []string{"No diagnosis"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	// The query's own engineering abstained; a later ticket sees it, even though
	// it is in another scope.
	later := fixture(e, "later")
	later.Versions = []string{"4.0.0"}
	receive(t, e, "later", later)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		if rows := unresolvedRecords(inputIssueRecords(t, e, in)); len(rows) != 1 || rows[0].SourceTicketID != "query" {
			t.Error("the engineering abstention was not exported", rows)
		}
		return Report{Summary: "Fixture clarification", Limitations: []string{"No diagnosis"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
}
