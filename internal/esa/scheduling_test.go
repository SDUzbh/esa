package esa

import (
	"context"
	"testing"
)

func TestV2ResumesNeverInvokedTaskWhenProviderAppears(t *testing.T) {
	e := v2Engine(t)
	receive(t, e, "ticket", fixture(e, "ticket"))
	if _, err := e.ProcessOne(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	task := sortedInvestigations(e.Store.Snapshot())[0]
	if task.State != "awaiting_configuration" || task.InvocationStarted {
		t.Fatal(task)
	}
	calls := 0
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { calls++; return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(e.Store.Snapshot().Investigations) != 1 {
		t.Fatal("provider change duplicated or suppressed the queued task")
	}
}
func TestV2ProjectRotationRetainsTaskOrder(t *testing.T) {
	e := v2Engine(t)
	a := fixture(e, "a")
	a.ProjectID = "1"
	b := fixture(e, "b")
	b.ProjectID = "2"
	receive(t, e, "a", a)
	receive(t, e, "b", b)
	s := e.Store.Snapshot()
	s.LastClaimedProject = "1"
	ordered := fairInvestigations(s)
	if ordered[0].ProjectID != "2" {
		t.Fatal("project rotation failed")
	}
}
