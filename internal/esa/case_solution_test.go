package esa

import (
	"context"
	"testing"
)

func TestSharedInvestigationsReviewedCaseSupersedesPendingAndSplitPreservesInputs(t *testing.T) {
	e := sharedInvestigationEngine(t)
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
	membership, _ := activeMembership(s, "member")
	r, err := e.ReviewMembership("member", MembershipReview{ExpectedMembershipID: membership.ID, ExpectedSnapshotID: memberSnapshot, Reviewer: "engineer", Reason: "Contradictory evidence requires a separate investigation"})
	if err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	if r.RunID != initial.ID || len(s.Investigations) != 1 || hash(s.Runs[initial.ID].Input) != hash(initial.Input) || len(s.MembershipHistory) < 4 {
		t.Fatal("split rewrote historical input or failed to retain membership revisions")
	}
	if _, err = e.ReviewMembership("member", MembershipReview{ExpectedMembershipID: membership.ID, ExpectedSnapshotID: memberSnapshot, Reviewer: "engineer", Reason: "Stale split"}); err == nil {
		t.Fatal("stale membership review accepted")
	}
}
