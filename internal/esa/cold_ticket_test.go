package esa

import (
	"encoding/json"
	"strings"
	"testing"
)

// closedAfterRestart receives an open and a closed ticket with text, then
// restarts, so the closed one is loaded without its text.
func closedAfterRestart(t *testing.T) (*Engine, Ticket, Ticket) {
	t.Helper()
	e, _, _, _ := pgEngine(t)
	open := fixture(e, "open")
	open.Description = "OPEN_TEXT consumer crashes"
	open.Comments = []Comment{{ID: "1", Body: "OPEN_COMMENT"}}
	receive(t, e, "open-event", open)
	closed := fixture(e, "closed")
	closed.Closed, closed.Status = true, "Resolved"
	closed.Description = "CLOSED_TEXT broker stopped"
	closed.Comments = []Comment{{ID: "1", Body: "CLOSED_COMMENT"}}
	closed.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	receive(t, e, "closed-event", closed)
	e.Store = reopenTestStore(t, e.Store)
	return e, open, closed
}

func TestClosedTicketTextStaysInStorageAfterRestart(t *testing.T) {
	e, open, closed := closedAfterRestart(t)
	s := e.Store.Snapshot()
	if got := s.Tickets.Get(closed.ID); got.Description != "" || got.Comments != nil || !got.textInStore || !got.Closed || got.Subject != closed.Subject {
		t.Fatal("a closed ticket was loaded with its text, or lost its metadata", got)
	}
	if got := s.Tickets.Get(open.ID); got.Description != open.Description || got.textInStore {
		t.Fatal("an open ticket was loaded without its text", got)
	}
	// The ticket API and the list read the text back from storage.
	w := classifierRequest(e, "GET", "/v1/tickets/"+closed.ID, "", "1", closed.ID+","+open.ID)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "CLOSED_TEXT") || !strings.Contains(w.Body.String(), "CLOSED_COMMENT") {
		t.Fatal("ticket GET lacks the stored text", w.Code, w.Body.String())
	}
	w = classifierRequest(e, "GET", "/v1/tickets", "", "1", closed.ID+","+open.ID)
	var list struct {
		Tickets []TicketRow `json:"tickets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 || len(list.Tickets) != 2 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	for _, row := range list.Tickets {
		want := map[string]string{open.ID: "OPEN_TEXT consumer crashes", closed.ID: "CLOSED_TEXT broker stopped"}[row.Ticket.ID]
		if row.Ticket.Description != want || len(row.Ticket.Comments) != 1 {
			t.Fatal("list row lacks the ticket text", row.Ticket)
		}
	}
}

func TestTicketLoadedWithoutTextIsNeverWritten(t *testing.T) {
	e, _, closed := closedAfterRestart(t)
	err := e.Store.Update(func(s *State) error {
		ticket := s.Tickets.Get(closed.ID)
		ticket.Status = "Edited in place"
		s.Tickets.Put(ticket.ID, ticket)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "loaded without its text") {
		t.Fatal("a ticket without its text was written", err)
	}
	full, err := e.Store.WithText(t.Context(), []Ticket{e.Store.Snapshot().Tickets.Get(closed.ID)})
	if err != nil || full[0].Description != closed.Description || full[0].Status == "Edited in place" {
		t.Fatal("the stored ticket changed", full, err)
	}
}

func TestReforwardingAClosedTicketAfterRestart(t *testing.T) {
	e, _, closed := closedAfterRestart(t)
	// Reopening forwards the full open ticket, which replaces the text-less one.
	reopened := closed
	reopened.Closed, reopened.Status = false, "Reopened"
	reopened.Description = "REOPENED_TEXT broker stopped again"
	reopened.UpdatedAt = reopened.UpdatedAt.Add(60e9)
	receive(t, e, "reopen-event", reopened)
	if got := e.Store.Snapshot().Tickets.Get(closed.ID); got.textInStore || got.Description != reopened.Description || got.Closed {
		t.Fatal("the reopened ticket was not replaced in full", got)
	}
	e.Store = reopenTestStore(t, e.Store)
	if got := e.Store.Snapshot().Tickets.Get(closed.ID); got.textInStore || got.Description != reopened.Description {
		t.Fatal("the reopened ticket lost its text across a restart", got)
	}
}

func TestClassifierUsesStoredTextOfAClosedTicket(t *testing.T) {
	e := classifierEngine(t)
	closed := fixture(e, "closed")
	closed.Closed, closed.Status = true, "Resolved"
	closed.Description = "Consumer crash null pointer during rebalance"
	closed.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	receive(t, e, "closed-event", closed)
	before := classifierRequest(e, "POST", "/v1/tickets/closed/classifier/predict", "{}", "1", "closed")
	e.Store = reopenTestStore(t, e.Store)
	if !e.Store.Snapshot().Tickets.Get("closed").textInStore {
		t.Fatal("the closed ticket kept its text across the restart")
	}
	w := classifierRequest(e, "POST", "/v1/tickets/closed/classifier/predict", "{}", "1", "closed")
	var a, b ClassifierPrediction
	if err := json.Unmarshal(before.Body.Bytes(), &a); err != nil || before.Code != 200 || w.Code != 200 {
		t.Fatal(before.Code, w.Code, w.Body.String(), err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Text != a.Text || b.ID != a.ID || !strings.Contains(b.Text, "null pointer") {
		t.Fatal("prediction after restart used other text", a.Text, b.Text, err)
	}
	req, _ := json.Marshal(classifierFeedbackRequest{ID: "feedback-1", PredictionID: b.ID, Labels: []string{"bug"}, Reviewer: "engineer"})
	if w = classifierRequest(e, "POST", "/v1/tickets/closed/classifier/feedback", string(req), "1", "closed"); w.Code != 200 {
		t.Fatal("feedback on a closed ticket after restart failed", w.Code, w.Body.String())
	}
}
