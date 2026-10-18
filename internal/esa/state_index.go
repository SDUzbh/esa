package esa

import (
	"fmt"
	"slices"
)

// tableIndex is a secondary index over one Table: buckets map an index key
// to the IDs whose value had that key in base, the table version the index
// was built from. A lookup stays exact at any later version, including in the
// middle of an Update: it adds the IDs that changed since base (diffTable,
// O(changes)) and keeps only IDs whose current value has the key. advance
// moves base forward by applying just those changes. Values whose key is ""
// are not indexed.
type tableIndex[V any] struct {
	base    Table[V]
	buckets Table[Table[struct{}]]
	built   bool
}

func buildTableIndex[V any](t Table[V], key func(V) string) tableIndex[V] {
	x := tableIndex[V]{base: t, built: true}
	for id, v := range t.All() {
		x.add(key(v), id)
	}
	return x
}

func (x *tableIndex[V]) add(k, id string) {
	if k == "" {
		return
	}
	bucket := x.buckets.Get(k)
	bucket.Put(id, struct{}{})
	x.buckets.Put(k, bucket)
}

func (x *tableIndex[V]) remove(k, id string) {
	if k == "" {
		return
	}
	bucket := x.buckets.Get(k)
	bucket.Delete(id)
	if bucket.Len() == 0 {
		x.buckets.Delete(k)
	} else {
		x.buckets.Put(k, bucket)
	}
}

// lookup returns, in sorted order, the IDs whose value in current has key k.
// An index that was never built falls back to scanning current.
func (x tableIndex[V]) lookup(current Table[V], key func(V) string, k string) []string {
	var ids []string
	if !x.built {
		for id, v := range current.All() {
			if key(v) == k {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return ids
	}
	candidates := map[string]bool{}
	for id := range x.buckets.Get(k).All() {
		candidates[id] = true
	}
	for _, id := range diffTable(x.base, current).Upserted {
		candidates[id] = true
	}
	for id := range candidates {
		if v, ok := current.Lookup(id); ok && key(v) == k {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// exists reports whether some ID whose value in current has key k also
// satisfies ok. It stops at the first match instead of collecting the bucket.
func (x tableIndex[V]) exists(current Table[V], key func(V) string, k string, ok func(V) bool) bool {
	match := func(id string) bool {
		v, found := current.Lookup(id)
		return found && key(v) == k && ok(v)
	}
	if !x.built {
		for _, v := range current.All() {
			if key(v) == k && ok(v) {
				return true
			}
		}
		return false
	}
	for id := range x.buckets.Get(k).All() {
		if match(id) {
			return true
		}
	}
	for _, id := range diffTable(x.base, current).Upserted {
		if match(id) {
			return true
		}
	}
	return false
}

// advance returns the index rebuilt incrementally for current.
func (x tableIndex[V]) advance(current Table[V], key func(V) string) tableIndex[V] {
	if !x.built {
		return buildTableIndex(current, key)
	}
	c := diffTable(x.base, current)
	for _, id := range append(c.Upserted, c.Deleted...) {
		if old, ok := x.base.Lookup(id); ok {
			x.remove(key(old), id)
		}
		if v, ok := current.Lookup(id); ok {
			x.add(key(v), id)
		}
	}
	x.base = current
	return x
}

func linkTicket(l IssueTicketLink) string        { return l.TicketID }
func linkInvestigation(l IssueTicketLink) string { return l.InvestigationID }
func taskIssue(t Investigation) string           { return t.IssueID }
func runTicket(r Run) string                     { return r.TicketID }
func caseTicket(c Case) string                   { return c.TicketID }
func overrideIssue(issueID string) string        { return issueID }
func groupIssue(g InvestigationGroup) string     { return g.IssueID }
func solutionTask(sol SolutionRevision) string   { return sol.InvestigationID }

// workState indexes investigations a worker may still claim, run or recover
// by their state; terminal investigations, the bulk of an archive, are not
// indexed.
func workState(t Investigation) string {
	switch t.State {
	case InvestigationQueued, InvestigationRunning, InvestigationInvestigating, InvestigationAwaitingConfiguration:
		return string(t.State)
	}
	return ""
}

func queuedRun(r Run) string {
	if r.State == RunQueued {
		return "queued"
	}
	return ""
}

// tentativeIssue, retainedCaseIssue and usableSolutionIssue index the records
// that can keep an Issue alive, judged by their own fields; Issue retention
// checks the ticket-dependent conditions at lookup time.
func tentativeIssue(l IssueTicketLink) string {
	if l.State != MembershipTentative {
		return ""
	}
	return l.IssueID
}

func retainedCaseIssue(c Case) string {
	if c.State == CaseInvalidated || c.State == CaseDuplicate || c.State == CaseArchived {
		return ""
	}
	return c.IssueID
}

func usableSolutionIssue(sol SolutionRevision) string {
	if sol.State != SolutionProposed && sol.State != SolutionTrusted {
		return ""
	}
	return sol.IssueID
}

// taskLiveApplication indexes investigations whose solution application can
// still be withdrawn.
func taskLiveApplication(t Investigation) string {
	if t.Application == nil || t.Application.State == ApplicationWithdrawn || t.Application.State == ApplicationRejected {
		return ""
	}
	return "live"
}

// stateIndexes are derived from State's tables and never persisted. The
// Store builds them on load and advances them after every commit; a State
// without them (a decoded copy, a test literal) falls back to scanning.
type stateIndexes struct {
	membershipsByTicket        tableIndex[IssueTicketLink]
	membershipsByInvestigation tableIndex[IssueTicketLink]
	investigationsByIssue      tableIndex[Investigation]
	liveApplications           tableIndex[Investigation]
	runsByTicket               tableIndex[Run]
	casesByTicket              tableIndex[Case]
	tentativeByIssue           tableIndex[IssueTicketLink]
	retainedCasesByIssue       tableIndex[Case]
	usableSolutionsByIssue     tableIndex[SolutionRevision]
	solutionsByInvestigation   tableIndex[SolutionRevision]
	overridesByIssue           tableIndex[string]
	groupsByIssue              tableIndex[InvestigationGroup]
	investigationsByWorkState  tableIndex[Investigation]
	queuedRuns                 tableIndex[Run]
}

func (s *State) advanceIndexes() {
	x := &s.indexes
	x.membershipsByTicket = x.membershipsByTicket.advance(s.Memberships, linkTicket)
	x.membershipsByInvestigation = x.membershipsByInvestigation.advance(s.Memberships, linkInvestigation)
	x.investigationsByIssue = x.investigationsByIssue.advance(s.Investigations, taskIssue)
	x.liveApplications = x.liveApplications.advance(s.Investigations, taskLiveApplication)
	x.runsByTicket = x.runsByTicket.advance(s.Runs, runTicket)
	x.casesByTicket = x.casesByTicket.advance(s.Cases, caseTicket)
	x.tentativeByIssue = x.tentativeByIssue.advance(s.Memberships, tentativeIssue)
	x.retainedCasesByIssue = x.retainedCasesByIssue.advance(s.Cases, retainedCaseIssue)
	x.usableSolutionsByIssue = x.usableSolutionsByIssue.advance(s.Solutions, usableSolutionIssue)
	x.solutionsByInvestigation = x.solutionsByInvestigation.advance(s.Solutions, solutionTask)
	x.overridesByIssue = x.overridesByIssue.advance(s.IssueOverrides, overrideIssue)
	x.groupsByIssue = x.groupsByIssue.advance(s.Groups, groupIssue)
	x.investigationsByWorkState = x.investigationsByWorkState.advance(s.Investigations, workState)
	x.queuedRuns = x.queuedRuns.advance(s.Runs, queuedRun)
}

func (s State) membershipsOfTicket(ticketID string) []string {
	return s.indexes.membershipsByTicket.lookup(s.Memberships, linkTicket, ticketID)
}

func (s State) membershipsOfInvestigation(taskID string) []string {
	return s.indexes.membershipsByInvestigation.lookup(s.Memberships, linkInvestigation, taskID)
}

func (s State) investigationsOfIssue(issueID string) []string {
	return s.indexes.investigationsByIssue.lookup(s.Investigations, taskIssue, issueID)
}

func (s State) runsOfTicket(ticketID string) []string {
	return s.indexes.runsByTicket.lookup(s.Runs, runTicket, ticketID)
}

func (s State) casesOfTicket(ticketID string) []string {
	return s.indexes.casesByTicket.lookup(s.Cases, caseTicket, ticketID)
}

func (s State) solutionsOfInvestigation(taskID string) []string {
	return s.indexes.solutionsByInvestigation.lookup(s.Solutions, solutionTask, taskID)
}

// investigationsIn returns the investigations in any of the given work
// states (see workState), ordered by ID.
func (s State) investigationsIn(states ...InvestigationState) []Investigation {
	var ids []string
	for _, state := range states {
		ids = append(ids, s.indexes.investigationsByWorkState.lookup(s.Investigations, workState, string(state))...)
	}
	slices.Sort(ids)
	rows := make([]Investigation, len(ids))
	for i, id := range ids {
		rows[i] = s.Investigations.Get(id)
	}
	return rows
}

// queuedRuns returns the queued runs, ordered by ID.
func (s State) queuedRuns() []Run {
	ids := s.indexes.queuedRuns.lookup(s.Runs, queuedRun, "queued")
	rows := make([]Run, len(ids))
	for i, id := range ids {
		rows[i] = s.Runs.Get(id)
	}
	return rows
}

func (s State) investigationsWithLiveApplication() []string {
	return s.indexes.liveApplications.lookup(s.Investigations, taskLiveApplication, "live")
}

// verifyIndexes is the guard's check that every advanced index matches a
// fresh build. It runs only while guardPublishedState is on.
func verifyIndexes(s *State) {
	if !guardPublishedState {
		return
	}
	check := func(name string, got, want Table[Table[struct{}]]) {
		a, _ := got.MarshalJSON()
		b, _ := want.MarshalJSON()
		if string(a) != string(b) {
			panic(fmt.Sprintf("esa: index %s drifted from its table", name))
		}
	}
	x := s.indexes
	check("membershipsByTicket", x.membershipsByTicket.buckets, buildTableIndex(s.Memberships, linkTicket).buckets)
	check("membershipsByInvestigation", x.membershipsByInvestigation.buckets, buildTableIndex(s.Memberships, linkInvestigation).buckets)
	check("investigationsByIssue", x.investigationsByIssue.buckets, buildTableIndex(s.Investigations, taskIssue).buckets)
	check("liveApplications", x.liveApplications.buckets, buildTableIndex(s.Investigations, taskLiveApplication).buckets)
	check("runsByTicket", x.runsByTicket.buckets, buildTableIndex(s.Runs, runTicket).buckets)
	check("casesByTicket", x.casesByTicket.buckets, buildTableIndex(s.Cases, caseTicket).buckets)
	check("tentativeByIssue", x.tentativeByIssue.buckets, buildTableIndex(s.Memberships, tentativeIssue).buckets)
	check("retainedCasesByIssue", x.retainedCasesByIssue.buckets, buildTableIndex(s.Cases, retainedCaseIssue).buckets)
	check("usableSolutionsByIssue", x.usableSolutionsByIssue.buckets, buildTableIndex(s.Solutions, usableSolutionIssue).buckets)
	check("solutionsByInvestigation", x.solutionsByInvestigation.buckets, buildTableIndex(s.Solutions, solutionTask).buckets)
	check("overridesByIssue", x.overridesByIssue.buckets, buildTableIndex(s.IssueOverrides, overrideIssue).buckets)
	check("groupsByIssue", x.groupsByIssue.buckets, buildTableIndex(s.Groups, groupIssue).buckets)
	check("investigationsByWorkState", x.investigationsByWorkState.buckets, buildTableIndex(s.Investigations, workState).buckets)
	check("queuedRuns", x.queuedRuns.buckets, buildTableIndex(s.Runs, queuedRun).buckets)
}
