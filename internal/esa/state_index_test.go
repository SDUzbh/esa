package esa

import (
	"slices"
	"testing"
)

// Index lookups inside an Update see that Update's own earlier writes:
// added, re-keyed and deleted rows, before the index is advanced.
func TestIndexLookupsSeeWritesInTheSameUpdate(t *testing.T) {
	e := testEngine(t)
	if err := e.Store.Update(func(s *State) error {
		s.Memberships.Put("l1", IssueTicketLink{ID: "l1", TicketID: "A", InvestigationID: "t1"})
		s.Memberships.Put("l2", IssueTicketLink{ID: "l2", TicketID: "A", InvestigationID: "t1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.Store.Snapshot().membershipsOfTicket("A"); !slices.Equal(got, []string{"l1", "l2"}) {
		t.Fatal("committed rows not indexed", got)
	}
	err := e.Store.Update(func(s *State) error {
		s.Memberships.Put("l3", IssueTicketLink{ID: "l3", TicketID: "A", InvestigationID: "t2"})
		moved := s.Memberships.Get("l2")
		moved.TicketID = "B"
		s.Memberships.Put("l2", moved)
		s.Memberships.Delete("l1")
		if got := s.membershipsOfTicket("A"); !slices.Equal(got, []string{"l3"}) {
			t.Error("ticket A in-flight", got)
		}
		if got := s.membershipsOfTicket("B"); !slices.Equal(got, []string{"l2"}) {
			t.Error("ticket B in-flight", got)
		}
		if got := s.membershipsOfInvestigation("t1"); !slices.Equal(got, []string{"l2"}) {
			t.Error("investigation t1 in-flight", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if got := s.membershipsOfTicket("A"); !slices.Equal(got, []string{"l3"}) {
		t.Fatal("ticket A after commit", got)
	}
	// A state without built indexes (for example a decoded copy) scans instead.
	if got := e.Store.Clone().membershipsOfTicket("B"); !slices.Equal(got, []string{"l2"}) {
		t.Fatal("unindexed fallback", got)
	}
}
