package esa

import (
	"context"
	"testing"
	"time"
)

func TestV2RetrievalStatisticsExcludeFutureCorpus(t *testing.T) {
	e := v2Engine(t)
	_, receipt := resolved(t, e)
	c, err := e.ReviewCase(receipt.CaseID, Review{Action: "approve", Reviewer: "engineer"})
	if err != nil {
		t.Fatal(err)
	}
	q := fixture(e, "query")
	cutoff := e.Now()
	before := MatchCasesV2(q, CasesAt([]Case{c}, cutoff))
	if len(before) != 1 || before[0].Retrieval == nil {
		t.Fatal("missing eligible retrieval trace")
	}
	future := c
	future.ID = "future"
	future.CreatedAt = cutoff.Add(time.Hour)
	future.UpdatedAt = future.CreatedAt
	future.TrustedAt = future.CreatedAt
	future.Summary = q.Description
	after := MatchCasesV2(q, CasesAt([]Case{c, future}, cutoff))
	if hash(before) != hash(after) {
		t.Fatal("future documents changed earlier ranking or corpus statistics")
	}
	other := c
	other.ID = "other-project"
	other.ProjectID = "2"
	other.Summary = q.Description
	if hash(MatchCasesV2(q, []Case{c, other})) != hash(before) {
		t.Fatal("other-project corpus changed scores")
	}
	unknown := c
	unknown.ID = "unknown"
	unknown.CreatedAt = time.Time{}
	if hash(MatchCasesV2(q, []Case{c, unknown})) != hash(before) {
		t.Fatal("unknown provenance entered strict corpus")
	}
}
func TestV2ReviewedCaseSupersedesPendingAndSplitPreservesInputs(t *testing.T) {
	e := v2Engine(t)
	q := fixture(e, "lead")
	receive(t, e, "lead", q)
	receive(t, e, "member", fixture(e, "member"))
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	task := sortedInvestigations(s)[0]
	old := s.Solutions[task.SolutionID]
	initial := s.Runs[task.RunID]
	q.Closed = true
	q.Resolution = Resolution{Cause: "Reviewed cause", Solution: "Engineer amendment", Validation: "Scoped validation"}
	receipt := receive(t, e, "resolved", q)
	if _, err := e.ReviewCase(receipt.CaseID, Review{Action: "approve", Reviewer: "engineer"}); err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	newTask := s.Investigations[task.ID]
	solution := s.Solutions[newTask.SolutionID]
	if solution.Origin != "engineer" || solution.State != "trusted" || solution.Resolution.Solution != "Engineer amendment" || solution.Supersedes != old.ID || s.Solutions[old.ID].State != "proposed" {
		t.Fatalf("review precedence: %+v", solution)
	}
	memberSnapshot := s.LatestSnapshot["member"]
	r, err := e.ReviewMembership("member", MembershipReview{ExpectedSnapshotID: memberSnapshot, Reviewer: "engineer", Reason: "Contradictory evidence requires a separate investigation"})
	if err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	if r.RunID == initial.ID || s.Runs[r.RunID].Route != "engineering_analysis" || hash(s.Runs[initial.ID].Input) != hash(initial.Input) || len(s.MembershipHistory) < 4 {
		t.Fatal("split rewrote historical input or failed to retain membership revisions")
	}
	if _, err = e.ReviewMembership("member", MembershipReview{ExpectedSnapshotID: memberSnapshot, Reviewer: "engineer", Reason: "Stale split"}); err == nil {
		t.Fatal("stale membership review accepted")
	}
}
