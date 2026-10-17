package esa

import (
	"testing"
)

// getTicketUpdates returns how many Store.Update calls one ticket GET made.
func getTicketUpdates(t *testing.T, e *Engine, id string) uint64 {
	t.Helper()
	before := e.Store.updates.Load()
	if w := classifierRequest(e, "GET", "/v1/tickets/"+id, "", "1", id); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	return e.Store.updates.Load() - before
}

func TestTicketGETSkipsWriteWhenNoIssueToTouch(t *testing.T) {
	e := testEngine(t)
	receive(t, e, "plain", fixture(e, "plain"))
	if n := getTicketUpdates(t, e, "plain"); n != 0 {
		t.Fatal("ticket without an Issue still attempted an access-order write", n)
	}
}

func TestTicketGETStillTouchesItsIssue(t *testing.T) {
	e := testEngine(t)
	ticket, run := analyzedTicket(t, e, "analyzed")
	if run.IssueID == "" {
		t.Fatal("fixture has no Issue")
	}
	before := e.Store.Snapshot().Issues.Get(run.IssueID).AccessOrder
	if n := getTicketUpdates(t, e, ticket.ID); n != 1 {
		t.Fatal("ticket with an Issue did not write its access order", n)
	}
	if after := e.Store.Snapshot().Issues.Get(run.IssueID).AccessOrder; after <= before {
		t.Fatal("Issue access order was not advanced", before, after)
	}
}
