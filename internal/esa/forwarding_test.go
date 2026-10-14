package esa

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManualForwardingRequiredBeforeIntake(t *testing.T) {
	e := testEngine(t)
	ticket := fixture(e, "manual")
	if _, err := e.Receive(Event{ID: "automatic", Origin: "redmine", Ticket: ticket}); err == nil {
		t.Fatal("automatic intake accepted")
	}
	if len(e.Store.Snapshot().Tickets) != 0 {
		t.Fatal("rejected intake changed store")
	}
	for _, subject := range []string{"How do I configure a consumer?", "Feature requested", "Unclassified failure"} {
		ticket.ID = subject
		ticket.Subject = subject
		receipt := receive(t, e, subject, ticket)
		run := e.Store.Snapshot().Runs[receipt.RunID]
		if run.Route != "engineering_analysis" {
			t.Fatal("classification still controls forwarding", run)
		}
	}
	if _, err := e.AnalyzeTicket(ticket.ID, false, ""); err == nil {
		t.Fatal("retry without human actor accepted")
	}
}
func TestLegacyAutomaticQueueCannotExecute(t *testing.T) {
	e := testEngine(t)
	receipt := receive(t, e, "old", fixture(e, "old"))
	e.Store.Update(func(s *State) error {
		r := s.Runs[receipt.RunID]
		r.WorkflowVersion = "2"
		r.Input.ForwardedBy = ""
		s.Runs[r.ID] = r
		return nil
	})
	called := false
	e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { called = true; return Report{}, nil }))
	if called || e.Store.Snapshot().Runs[receipt.RunID].State != "awaiting_forward" {
		t.Fatal("old automatic queue executed")
	}
}
func TestClassificationEndpointsRemoved(t *testing.T) {
	e := testEngine(t)
	handler := (&API{Engine: e, Token: "token"}).Handler()
	for _, path := range []string{"/v1/labels", "/v1/labels/examples"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-ESA-Project-IDs", "1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	ticket := fixture(e, "legacy-record")
	e.Store.Update(func(s *State) error { s.Tickets[ticket.ID] = ticket; return nil })
	r := httptest.NewRequest("GET", "/v1/tickets", nil)
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("X-ESA-Project-IDs", "1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"total_count":0`) {
		t.Fatal("legacy unforwarded ticket listed", w.Body.String())
	}
}
func TestForceForwardIsOneRunAndIdempotent(t *testing.T) {
	e := testEngine(t)
	ticket := fixture(e, "force")
	first := receive(t, e, "first", ticket)
	event := Event{ID: "force", Origin: "manual_forward", ForwardedBy: "human", ForwardedAt: e.Now(), Ticket: ticket, Force: true}
	second, err := e.Receive(event)
	if err != nil {
		t.Fatal(err)
	}
	third, err := e.Receive(event)
	if err != nil || first.RunID == second.RunID || third.RunID != second.RunID || third.Action != "duplicate" || len(e.Store.Snapshot().Runs) != 2 {
		t.Fatal(first, second, third, err)
	}
	b, _ := json.Marshal(e.Store.Snapshot().Runs[second.RunID])
	if strings.Contains(string(b), `"classification"`) {
		t.Fatal("new run contains classification", string(b))
	}
}
