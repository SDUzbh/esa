package esa

import (
	"reflect"
	"testing"
)

// Every map and slice field of State must be detached, including ones added
// later; otherwise an Update callback would write into the published state.
func TestDetachCopiesEveryTopLevelContainer(t *testing.T) {
	var s State
	v := reflect.ValueOf(&s).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Map:
			f.Set(reflect.MakeMap(f.Type()))
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 4))
		}
	}
	next := s.detach()
	n := reflect.ValueOf(next)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		switch f := v.Field(i); f.Kind() {
		case reflect.Map:
			if n.Field(i).UnsafePointer() == f.UnsafePointer() {
				t.Errorf("State.%s map is shared after detach", name)
			}
		case reflect.Slice:
			if n.Field(i).Cap() != n.Field(i).Len() {
				t.Errorf("State.%s slice keeps spare capacity after detach", name)
			}
		}
	}
}

func TestUpdateFailureLeavesPublishedStateAndEncodingIntact(t *testing.T) {
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	before := e.Store.Snapshot()
	err := e.Store.Update(func(s *State) error {
		s.Tickets["T2"] = fixture(e, "T2")
		delete(s.Tickets, "T1")
		s.CaseHistory = append(s.CaseHistory, Case{ID: "discarded"})
		return ErrConflict
	})
	if err != ErrConflict {
		t.Fatal(err)
	}
	after := e.Store.Snapshot()
	if _, ok := after.Tickets["T1"]; !ok || len(after.Tickets) != len(before.Tickets) || len(after.CaseHistory) != len(before.CaseHistory) {
		t.Fatal("failed Update changed the published state")
	}
	// A no-op after a failed Update must still be detected as unchanged.
	if err = e.Store.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
