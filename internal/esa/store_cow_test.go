package esa

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// Every map and slice field of State must be detached, including ones added
// later; otherwise an Update callback would write into the published state.
// Writing any table or slice of a detached State must leave the original
// unchanged, including fields added later.
func TestDetachCopiesEveryTopLevelContainer(t *testing.T) {
	var s State
	v := reflect.ValueOf(&s).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch {
		case f.Kind() == reflect.Map:
			f.Set(reflect.MakeMap(f.Type()))
		case f.Kind() == reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 4))
		}
	}
	next := s.detach()
	n := reflect.ValueOf(&next).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		f, nf := v.Field(i), n.Field(i)
		if table, ok := nf.Addr().Interface().(stateTable); ok {
			table.tablePutZero("written-after-detach")
			if keys := f.Addr().Interface().(stateTable).tableKeys(); len(keys) != 0 {
				t.Errorf("State.%s: writing the detached table changed the original", name)
			}
			continue
		}
		switch f.Kind() {
		case reflect.Map:
			if nf.UnsafePointer() == f.UnsafePointer() {
				t.Errorf("State.%s map is shared after detach", name)
			}
		case reflect.Slice:
			before, _ := json.Marshal(f.Interface())
			nf.Set(reflect.Append(nf, reflect.Zero(nf.Type().Elem())))
			if after, _ := json.Marshal(f.Interface()); f.Len() != 1 || string(after) != string(before) {
				t.Errorf("State.%s: appending to the detached history changed the original", name)
			}
		}
	}
}

func TestUpdateFailureLeavesPublishedStateAndEncodingIntact(t *testing.T) {
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	before := e.Store.Snapshot()
	err := e.Store.Update(func(s *State) error {
		s.Tickets.Put("T2", fixture(e, "T2"))
		s.Tickets.Delete("T1")
		s.CaseHistory = append(s.CaseHistory, Case{ID: "discarded"})
		return ErrConflict
	})
	if err != ErrConflict {
		t.Fatal(err)
	}
	after := e.Store.Snapshot()
	if _, ok := after.Tickets.Lookup("T1"); !ok || after.Tickets.Len() != before.Tickets.Len() || len(after.CaseHistory) != len(before.CaseHistory) {
		t.Fatal("failed Update changed the published state")
	}
	// A no-op after a failed Update must still be detected as unchanged.
	if err = e.Store.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// A reader's snapshot keeps its history while later Updates append, and an
// append from a failed Update is replaced by the next successful one.
func TestHistoryAppendsAreInvisibleToEarlierSnapshots(t *testing.T) {
	e := testEngine(t)
	add := func(id string, fail bool) error {
		return e.Store.Update(func(s *State) error {
			s.CaseHistory = append(s.CaseHistory, Case{ID: id})
			if fail {
				return ErrConflict
			}
			return nil
		})
	}
	for i := 0; i < 3; i++ {
		if err := add(fmt.Sprint("c", i), false); err != nil {
			t.Fatal(err)
		}
	}
	reader := e.Store.Snapshot()
	if err := add("failed", true); err != ErrConflict {
		t.Fatal(err)
	}
	if err := add("c3", false); err != nil {
		t.Fatal(err)
	}
	if len(reader.CaseHistory) != 3 || reader.CaseHistory[2].ID != "c2" {
		t.Fatal("an earlier snapshot saw a later append", reader.CaseHistory)
	}
	got := e.Store.Snapshot().CaseHistory
	if len(got) != 4 || got[3].ID != "c3" {
		t.Fatal("history lost or kept the failed append", got)
	}
	if c := diffHistory(reader.CaseHistory, got); c != (HistoryChange{Appended: 1}) {
		t.Fatal("shared-prefix diff", c)
	}
}
