package esa

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// legacyStateFile writes a state whose events use the old shape, with a full
// ticket copy in every event, as files written before EventRecord did.
func legacyStateFile(t *testing.T, s State, tamper string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]json.RawMessage
	if err = json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]Event{}
	for id, r := range s.Events.All() {
		ticket := s.Snapshots.Get(r.SnapshotID).Ticket
		if r.Ticket != nil {
			ticket = *r.Ticket
		}
		if id == tamper {
			ticket.Subject = "diverges from its snapshot"
		}
		legacy[id] = Event{ID: r.ID, Origin: r.Origin, Force: r.Force, ForwardedBy: r.ForwardedBy, ForwardedAt: r.ForwardedAt, OccurredAt: r.OccurredAt, ReceivedAt: r.ReceivedAt, Ticket: ticket, Raw: r.Raw}
	}
	if frame["events"], err = json.Marshal(legacy); err != nil {
		t.Fatal(err)
	}
	if raw, err = json.Marshal(frame); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLegacyEventsConvertOnLoad(t *testing.T) {
	e := testEngine(t)
	accepted := fixture(e, "T1")
	receive(t, e, "accepted", accepted)
	receive(t, e, "divergent", fixture(e, "T2"))
	stale := accepted
	stale.Subject, stale.UpdatedAt = "Older edit", accepted.UpdatedAt.Add(-time.Hour)
	if _, err := e.Receive(forward(e, "stale", stale)); err != nil {
		t.Fatal(err)
	}
	path := legacyStateFile(t, e.Store.Snapshot(), "divergent")
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"accepted":{"forwarded_by"`) || !strings.Contains(string(b), `"ticket":{`) {
		t.Fatal("fixture is not in the legacy event shape")
	}
	loaded, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s := loaded.Snapshot()
	if r := s.Events.Get("accepted"); r.Ticket != nil || r.SnapshotID == "" || r.TicketHash != s.Snapshots.Get(r.SnapshotID).Hash || r.TicketID != "T1" {
		t.Fatal("legacy event with a matching snapshot was not compacted", r)
	}
	if r := s.Events.Get("stale"); r.Ticket == nil || r.SnapshotID != "" || r.Ticket.Subject != "Older edit" {
		t.Fatal("legacy event without a snapshot lost its ticket", r)
	}
	if r := s.Events.Get("divergent"); r.Ticket == nil || r.Ticket.Subject != "diverges from its snapshot" || r.TicketHash != hash(*r.Ticket) {
		t.Fatal("legacy event that differs from its snapshot lost its own ticket", r)
	}
	// Idempotency keeps working on converted records.
	reloaded := testEngine(t)
	reloaded.Store = loaded
	original := Event{ID: "accepted", Origin: "test", ForwardedBy: "fixture-human", ForwardedAt: e.Now(), Ticket: accepted}
	if receipt, err := reloaded.Receive(original); err != nil || receipt.Action != "duplicate" {
		t.Fatal("converted event no longer deduplicates", receipt.Action, err)
	}
	changed := original
	changed.Ticket.Subject = "Different content"
	if _, err := reloaded.Receive(changed); !errors.Is(err, ErrRejected) {
		t.Fatal("converted event no longer rejects a reused ID with different content", err)
	}
	// The next write stores the new shape.
	if err = loaded.Update(func(st *State) error { st.LastClaimedProject = "1"; return nil }); err != nil {
		t.Fatal(err)
	}
	var written struct {
		Events map[string]map[string]json.RawMessage `json:"events"`
	}
	b, _ := os.ReadFile(path)
	if err = json.Unmarshal(b, &written); err != nil {
		t.Fatal(err)
	}
	if _, copied := written.Events["accepted"]["ticket"]; copied || len(written.Events["accepted"]["ticket_hash"]) == 0 {
		t.Fatal("rewritten file still copies the ticket into an accepted event")
	}
}
