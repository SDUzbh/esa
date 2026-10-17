package esa

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func forward(e *Engine, id string, ticket Ticket) Event {
	return Event{ID: id, Origin: "manual_forward", ForwardedBy: "support-user", ForwardedAt: e.Now(), Ticket: ticket}
}

func TestReceiveStoresEventRecordWithoutTicketCopy(t *testing.T) {
	e := testEngine(t)
	event := forward(e, "e1", fixture(e, "T1"))
	event.Raw = json.RawMessage(`{"source":"jira"}`)
	if _, err := e.Receive(event); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	r := s.Events.Get("e1")
	snap, ok := s.Snapshots.Lookup(r.SnapshotID)
	if !ok || r.SnapshotID != s.LatestSnapshot.Get("T1") || r.Ticket != nil {
		t.Fatal("accepted event must reference its snapshot instead of copying the ticket", r.SnapshotID, r.Ticket != nil)
	}
	if r.TicketID != "T1" || r.TicketHash != snap.Hash || r.ForwardedBy != "support-user" || r.Origin != "manual_forward" || string(r.Raw) != `{"source":"jira"}` {
		t.Fatal("event metadata or Raw not kept", r)
	}
}

func TestReceiveKeepsTicketForEventsWithoutSnapshot(t *testing.T) {
	e := testEngine(t)
	current := fixture(e, "T1")
	receive(t, e, "current", current)
	stale := current
	stale.Subject, stale.UpdatedAt = "Older edit", current.UpdatedAt.Add(-time.Hour)
	self := current
	self.Subject = "Echo of an ESA write"
	selfEvent := forward(e, "self", self)
	selfEvent.Origin = "esa"
	for _, ev := range []Event{forward(e, "stale", stale), selfEvent} {
		receipt, err := e.Receive(ev)
		if err != nil || !strings.HasPrefix(receipt.Action, "ignored_") {
			t.Fatal("expected an ignored event", ev.ID, receipt.Action, err)
		}
		r := e.Store.Snapshot().Events.Get(ev.ID)
		if r.SnapshotID != "" || r.Ticket == nil || r.Ticket.Subject != ev.Ticket.Subject || r.TicketHash != hash(*r.Ticket) {
			t.Fatal("an event without a snapshot must keep its ticket", ev.ID, r)
		}
	}
}

func TestEventRecordIdempotency(t *testing.T) {
	e := testEngine(t)
	event := forward(e, "e1", fixture(e, "T1"))
	if _, err := e.Receive(event); err != nil {
		t.Fatal(err)
	}
	if receipt, err := e.Receive(event); err != nil || receipt.Action != "duplicate" {
		t.Fatal("same event ID and content must be a duplicate", receipt.Action, err)
	}
	changed := event
	changed.Ticket.Subject = "Different content"
	if _, err := e.Receive(changed); !errors.Is(err, ErrConflict) || !errors.Is(err, ErrRejected) {
		t.Fatal("reused event ID with different content must be rejected", err)
	}
}
