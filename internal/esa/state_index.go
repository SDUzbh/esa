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

// taskLiveApplication indexes investigations whose solution application can
// still be withdrawn.
func taskLiveApplication(t Investigation) string {
	if t.Application == nil || t.Application.State == "withdrawn" || t.Application.State == "rejected" {
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
}

func (s *State) advanceIndexes() {
	x := &s.indexes
	x.membershipsByTicket = x.membershipsByTicket.advance(s.Memberships, linkTicket)
	x.membershipsByInvestigation = x.membershipsByInvestigation.advance(s.Memberships, linkInvestigation)
	x.investigationsByIssue = x.investigationsByIssue.advance(s.Investigations, taskIssue)
	x.liveApplications = x.liveApplications.advance(s.Investigations, taskLiveApplication)
	x.runsByTicket = x.runsByTicket.advance(s.Runs, runTicket)
	x.casesByTicket = x.casesByTicket.advance(s.Cases, caseTicket)
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
}
