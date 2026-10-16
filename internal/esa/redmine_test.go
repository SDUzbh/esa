package esa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOutbox mirrors the plugin: index returns unacknowledged events in id order
// and ack removes one. dropAcks simulates an acknowledgement lost in transit.
type fakeOutbox struct {
	mu       sync.Mutex
	events   []Event
	acked    []string
	bodies   []map[string]string
	dropAcks bool
}

// validAck accepts only the bodies ESA sends: an accepted status, or a rejected
// status with a reason.
func validAck(body map[string]string) bool {
	switch body["status"] {
	case "accepted":
		return len(body) == 2
	case "rejected":
		return len(body) == 3 && body["reason"] != ""
	}
	return false
}

func (o *fakeOutbox) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if r.URL.Path == "/esa/api/events/ack" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["event_id"] == "" || !validAck(body) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			o.acked = append(o.acked, body["event_id"])
			o.bodies = append(o.bodies, body)
			if !o.dropAcks {
				for i, event := range o.events {
					if event.ID == body["event_id"] {
						o.events = append(o.events[:i], o.events[i+1:]...)
						break
					}
				}
			}
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": o.events})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func outboxEvent(e *Engine, id string, ticket Ticket) Event {
	return Event{ID: id, Origin: "manual_forward", ForwardedBy: "support-user", ForwardedAt: e.Now(), Ticket: ticket}
}

func sync3(e *Engine, srv *httptest.Server) error {
	var err error
	for i := 0; i < 3; i++ {
		err = e.SyncRedmine(context.Background(), srv.URL, "token", srv.Client())
	}
	return err
}

func TestSyncRedminePoisonEventDoesNotBlockLaterEvents(t *testing.T) {
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	moved := fixture(e, "T1")
	moved.ProjectID = "2" // The Redmine issue moved to another project and was forwarded again.
	outbox := &fakeOutbox{events: []Event{outboxEvent(e, "redmine_event_1", moved), outboxEvent(e, "redmine_event_2", fixture(e, "T2"))}}
	if err := sync3(e, outbox.serve(t)); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if _, ok := s.Tickets["T2"]; !ok {
		t.Fatal("valid event behind a permanently rejected event was never received")
	}
	if strings.Join(outbox.acked, ",") != "redmine_event_1,redmine_event_2" {
		t.Fatal("each event must be acknowledged exactly once", outbox.acked)
	}
	rejection, ok := s.Rejections["redmine_event_1"]
	if !ok || rejection.TicketID != "T1" || rejection.ProjectID != "2" || rejection.ForwardedBy != "support-user" || !strings.Contains(rejection.Reason, "different project") || !rejection.RejectedAt.Equal(e.Now()) {
		t.Fatal("rejection was not recorded", rejection)
	}
	if s.Tickets["T1"].ProjectID != "1" || len(s.Rejections) != 1 {
		t.Fatal("a rejected event must not change accepted state", s.Tickets["T1"].ProjectID, s.Rejections)
	}
	if _, ok := s.Events["redmine_event_1"]; ok {
		t.Fatal("a rejected event must not be stored as accepted")
	}
}

func TestSyncRedmineAcknowledgementReportsOutcome(t *testing.T) {
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	moved := fixture(e, "T1")
	moved.ProjectID = "2"
	outbox := &fakeOutbox{events: []Event{outboxEvent(e, "redmine_event_1", moved), outboxEvent(e, "redmine_event_2", fixture(e, "T2"))}}
	if err := sync3(e, outbox.serve(t)); err != nil {
		t.Fatal(err)
	}
	if len(outbox.bodies) != 2 {
		t.Fatal("each event must be acknowledged exactly once", outbox.bodies)
	}
	rejected, accepted := outbox.bodies[0], outbox.bodies[1]
	if rejected["event_id"] != "redmine_event_1" || rejected["status"] != "rejected" || rejected["reason"] != "conflict: ticket belongs to a different project" {
		t.Fatal("rejected acknowledgement must carry the status and reason", rejected)
	}
	if rejected["reason"] != e.Store.Snapshot().Rejections["redmine_event_1"].Reason {
		t.Fatal("acknowledged reason must match the recorded rejection", rejected)
	}
	if accepted["event_id"] != "redmine_event_2" || accepted["status"] != "accepted" || accepted["reason"] != "" {
		t.Fatal("accepted acknowledgement must carry only the accepted status", accepted)
	}
}

func TestSyncRedmineRejectionSurvivesLostAcknowledgement(t *testing.T) {
	e := testEngine(t)
	invalid := fixture(e, "T1")
	invalid.UpdatedAt = e.Now().Add(time.Hour) // Snapshot newer than its forwarding cutoff.
	outbox := &fakeOutbox{events: []Event{outboxEvent(e, "redmine_event_1", invalid)}, dropAcks: true}
	srv := outbox.serve(t)
	if err := sync3(e, srv); err != nil {
		t.Fatal(err)
	}
	first := e.Store.Snapshot().Rejections["redmine_event_1"]
	e.Now = func() time.Time { return first.RejectedAt.Add(time.Minute) }
	if err := e.SyncRedmine(context.Background(), srv.URL, "token", srv.Client()); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if len(outbox.acked) != 4 || len(s.Rejections) != 1 || s.Rejections["redmine_event_1"] != first || !strings.Contains(first.Reason, "forwarding cutoff") {
		t.Fatal("redelivered rejection must be acknowledged again without a new record", outbox.acked, s.Rejections)
	}
}

func TestSyncRedmineRejectsReusedEventIDWithoutTouchingOriginal(t *testing.T) {
	e := testEngine(t)
	original := fixture(e, "T1")
	receive(t, e, "redmine_event_1", original)
	changed := original
	changed.Subject = "Different content under the same event ID"
	reused := outboxEvent(e, "redmine_event_1", changed)
	reused.Origin = "test"
	outbox := &fakeOutbox{events: []Event{reused, outboxEvent(e, "redmine_event_2", fixture(e, "T2"))}}
	if err := sync3(e, outbox.serve(t)); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if s.Events["redmine_event_1"].Ticket.Subject != original.Subject || s.Receipts["redmine_event_1"].Action == "" {
		t.Fatal("original accepted event or receipt changed")
	}
	if !strings.Contains(s.Rejections["redmine_event_1"].Reason, "reused with different content") || len(outbox.acked) != 2 {
		t.Fatal("reused event ID was not rejected and acknowledged", s.Rejections, outbox.acked)
	}
}

func TestSyncRedmineTransientFailureHoldsOnlyItsTicket(t *testing.T) {
	e := testEngine(t)
	now := e.Now()
	early := fixture(e, "T1")
	ahead := outboxEvent(e, "redmine_event_1", early)
	ahead.ForwardedAt = now.Add(30 * time.Second) // Redmine clock slightly ahead of ESA.
	later := fixture(e, "T1")
	later.Subject, later.UpdatedAt = "Later edit", now.Add(time.Minute)
	next := outboxEvent(e, "redmine_event_2", later)
	next.ForwardedAt = now.Add(time.Minute)
	outbox := &fakeOutbox{events: []Event{ahead, next, outboxEvent(e, "redmine_event_3", fixture(e, "T2"))}}
	srv := outbox.serve(t)
	err := e.SyncRedmine(context.Background(), srv.URL, "token", srv.Client())
	if err == nil || errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "redmine_event_1") || strings.Contains(err.Error(), "redmine_event_2") {
		t.Fatal("transient failure must be reported once and must not be a rejection", err)
	}
	s := e.Store.Snapshot()
	if _, ok := s.Tickets["T1"]; ok || len(s.Rejections) != 0 || strings.Join(outbox.acked, ",") != "redmine_event_3" {
		t.Fatal("only other tickets may pass a transient failure", outbox.acked, s.Rejections)
	}
	if _, ok := s.Tickets["T2"]; !ok {
		t.Fatal("other ticket was blocked by a transient failure")
	}
	e.Now = func() time.Time { return now.Add(2 * time.Minute) }
	if err = e.SyncRedmine(context.Background(), srv.URL, "token", srv.Client()); err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	if strings.Join(outbox.acked, ",") != "redmine_event_3,redmine_event_1,redmine_event_2" || s.Tickets["T1"].Subject != "Later edit" || s.Receipts["redmine_event_2"].Action == "ignored_stale_event" {
		t.Fatal("held events must be applied in their original per-ticket order", outbox.acked, s.Tickets["T1"].Subject, s.Receipts["redmine_event_2"])
	}
}

func TestReceiveClassifiesPermanentRejections(t *testing.T) {
	e := testEngine(t)
	missing := fixture(e, "T1")
	missing.Subject = ""
	future := outboxEvent(e, "future", fixture(e, "T2"))
	future.ForwardedAt = e.Now().Add(time.Second)
	cases := []struct {
		event     Event
		permanent bool
		status    error
	}{
		{outboxEvent(e, "missing", missing), true, ErrInvalid},
		{Event{ID: "anonymous", Origin: "manual_forward", ForwardedAt: e.Now(), Ticket: fixture(e, "T3")}, true, ErrInvalid},
		{future, false, ErrInvalid},
	}
	for _, c := range cases {
		_, err := e.Receive(c.event)
		if err == nil || errors.Is(err, ErrRejected) != c.permanent || !errors.Is(err, c.status) {
			t.Fatal("misclassified intake error", c.event.ID, err)
		}
	}
	// Status mapping and messages seen by HTTP callers are unchanged.
	if _, err := e.Receive(outboxEvent(e, "missing", missing)); err.Error() != "invalid request: event ID, ticket ID, project ID, and subject are required" {
		t.Fatal("intake error message changed", err)
	}
}
