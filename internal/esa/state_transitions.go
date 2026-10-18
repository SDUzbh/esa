package esa

import "fmt"

// verifyTransitions is the guard's check that every state change an Update
// commits is listed in states.go. It runs only while guardPublishedState is on.
func verifyTransitions(old, next *State, changes StateChanges) {
	if !guardPublishedState {
		return
	}
	for _, id := range changes.Maps["investigations"].Upserted {
		before, existed := old.Investigations.Lookup(id)
		after := next.Investigations.Get(id)
		checkTransition("investigation", investigationTransitions, existed, before.State, after.State)
		if after.Application != nil {
			var from ApplicationState
			if before.Application != nil {
				from = before.Application.State
			}
			checkTransition("application", applicationTransitions, before.Application != nil, from, after.Application.State)
		}
	}
	for _, id := range changes.Maps["runs"].Upserted {
		before, existed := old.Runs.Lookup(id)
		checkTransition("run", runTransitions, existed, before.State, next.Runs.Get(id).State)
	}
	for _, id := range changes.Maps["cases"].Upserted {
		before, existed := old.Cases.Lookup(id)
		checkTransition("case", caseTransitions, existed, before.State, next.Cases.Get(id).State)
	}
	for _, id := range changes.Maps["solution_revisions"].Upserted {
		before, existed := old.Solutions.Lookup(id)
		checkTransition("solution", solutionTransitions, existed, before.State, next.Solutions.Get(id).State)
	}
	for _, id := range changes.Maps["issue_ticket_links"].Upserted {
		before, existed := old.Memberships.Lookup(id)
		checkTransition("membership", membershipTransitions, existed, before.State, next.Memberships.Get(id).State)
	}
}

func checkTransition[S ~string](entity string, table transitions[S], existed bool, from, to S) {
	if !existed {
		from = ""
	}
	if from != to && !table.allows(from, to) {
		panic(fmt.Sprintf("esa: %s state changed %q -> %q, which states.go does not allow; add the transition there if this path is deliberate", entity, from, to))
	}
}
