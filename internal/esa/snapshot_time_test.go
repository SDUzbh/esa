package esa

import (
	"testing"
	"time"
)

func TestSharedInvestigationsOutboxRecordingAfterForwardingDoesNotLookLikeFutureSource(t *testing.T) {
	e := sharedInvestigationEngine(t)
	ticket := fixture(e, "outbox")
	_, err := e.Receive(Event{ID: "outbox", Origin: "manual_forward", ForwardedBy: "human", ForwardedAt: e.Now(), OccurredAt: e.Now().Add(time.Millisecond), Ticket: ticket})
	if err != nil {
		t.Fatal("legitimate outbox recording time was confused with source availability", err)
	}
	updated := fixture(e, "updated-comment")
	updated.Comments = []Comment{{ID: "comment", Body: "Future edit", CreatedAt: e.Now().Add(-time.Hour), UpdatedAt: e.Now().Add(time.Hour)}}
	if _, err = e.Receive(Event{ID: "future-comment", ForwardedBy: "human", ForwardedAt: e.Now(), Ticket: updated}); err == nil {
		t.Fatal("future comment edit accepted")
	}
}
