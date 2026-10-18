package esa

import (
	"strings"
	"testing"
)

// The guard rejects a state change states.go does not list.
func TestGuardRejectsAnUnlistedTransition(t *testing.T) {
	e := testEngine(t)
	ticket := fixture(e, "closed")
	ticket.Closed, ticket.Status = true, "Resolved"
	ticket.Resolution = Resolution{Reason: "rejected", Cause: "c", Solution: "s", Validation: "v"}
	r := receive(t, e, "closed", ticket)
	if got := e.Store.Snapshot().Cases.Get(r.CaseID).State; got != CaseArchived {
		t.Fatal("setup: expected an archived case, got", got)
	}
	defer func() {
		if v := recover(); v == nil || !strings.Contains(v.(string), `case state changed "archived" -> "trusted"`) {
			t.Fatal("an unlisted transition was committed:", v)
		}
	}()
	_ = e.Store.Update(func(s *State) error {
		c := s.Cases.Get(r.CaseID)
		c.State = CaseTrusted
		s.Cases.Put(c.ID, c)
		return nil
	})
}

func TestTransitionTablesUseOnlyTheirOwnStates(t *testing.T) {
	if !investigationTransitions.allows(InvestigationQueued, InvestigationRunning) || investigationTransitions.allows(InvestigationFailed, InvestigationQueued) {
		t.Fatal("investigation table lookup is wrong")
	}
	if !caseTransitions.allows("", CaseDraft) || caseTransitions.allows(CaseArchived, CaseTrusted) {
		t.Fatal("case table lookup is wrong")
	}
}
