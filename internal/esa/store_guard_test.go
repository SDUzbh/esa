package esa

import (
	"strings"
	"testing"
)

func expectGuardPanic(t *testing.T, operation func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "mutated in place") {
			t.Fatal("published-state guard did not fire", r)
		}
	}()
	operation()
}

func TestPublishedStateGuardDetectsInPlaceMutation(t *testing.T) {
	if !guardPublishedState {
		t.Skip("guard is off while benchmarking")
	}
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	shared := e.Store.state.Tickets.Get("T1").Environment
	original := shared["details"]
	// Simulate code that edits a nested map shared with the published state.
	shared["details"] = "edited in place"
	expectGuardPanic(t, func() { _ = e.Store.Update(func(*State) error { return nil }) })
	expectGuardPanic(t, func() { _ = e.Store.Snapshot() })
	shared["details"] = original
	if err := e.Store.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedStateGuardDetectsCallbackEditingSharedValue(t *testing.T) {
	if !guardPublishedState {
		t.Skip("guard is off while benchmarking")
	}
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	shared := e.Store.state.Tickets.Get("T1").Environment
	original := shared["details"]
	expectGuardPanic(t, func() {
		_ = e.Store.Update(func(*State) error {
			shared["details"] = "edited by a callback"
			return nil
		})
	})
	shared["details"] = original
}
