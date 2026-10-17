package esa

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDiffMapReportsOnlyRealChanges(t *testing.T) {
	e := testEngine(t)
	old := map[string]Ticket{"a": fixture(e, "a"), "b": fixture(e, "b"), "c": fixture(e, "c")}
	next := cloneTickets(old)
	next["a"] = next["a"] // identical replace
	changed := next["b"]
	changed.Assignee = "someone"
	next["b"] = changed
	delete(next, "c")
	next["d"] = fixture(e, "d")
	got := diffMap(old, next)
	if !slices.Equal(got.Upserted, []string{"b", "d"}) || !slices.Equal(got.Deleted, []string{"c"}) {
		t.Fatal("unexpected map changes", got)
	}
	if got := diffMap(old, cloneTickets(old)); !got.empty() {
		t.Fatal("untouched copy reported changes", got)
	}
}

func cloneTickets(m map[string]Ticket) map[string]Ticket {
	out := make(map[string]Ticket, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestDiffHistoryTailAndRewrite(t *testing.T) {
	old := []Case{{ID: "1"}, {ID: "2"}}
	if got := diffHistory(old, slices.Clip(old)); got != (HistoryChange{}) {
		t.Fatal("unchanged history reported", got)
	}
	if got := diffHistory(old, append(slices.Clip(old), Case{ID: "3"}, Case{ID: "4"})); got != (HistoryChange{Appended: 2}) {
		t.Fatal("append not reported as tail", got)
	}
	rewritten := []Case{{ID: "1"}, {ID: "changed"}, {ID: "3"}}
	if got := diffHistory(old, rewritten); !got.Rewritten {
		t.Fatal("rewritten prefix not reported", got)
	}
	if got := diffHistory(old, old[:1]); !got.Rewritten {
		t.Fatal("shrunk history not reported", got)
	}
}

func TestDiffFrameFields(t *testing.T) {
	base := State{SchemaVersion: 1, StoredFields: JSONFields{"legacy": json.RawMessage(`1`)}}
	for name, edit := range map[string]func(*State){
		"access sequence": func(s *State) { s.IssueAccessSequence++ },
		"replay clock":    func(s *State) { s.ReplayClock = time.Unix(1, 0).UTC() },
		"last project":    func(s *State) { s.LastClaimedProject = "2" },
		"schema version":  func(s *State) { s.SchemaVersion = 2 },
		"stored fields":   func(s *State) { s.StoredFields["legacy"] = json.RawMessage(`2`) },
	} {
		next := base.detach()
		edit(&next)
		if c := diffState(&base, &next); !c.Frame {
			t.Error("frame change not detected:", name)
		}
	}
	next := base.detach()
	if c := diffState(&base, &next); !c.Empty() {
		t.Fatal("detached state without edits reported changes", c)
	}
}

// Every State field must be covered by diffState, including fields added later.
func TestDiffStateCoversEveryField(t *testing.T) {
	var base State
	v := reflect.ValueOf(&base).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Map {
			f.Set(reflect.MakeMap(f.Type()))
		}
	}
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		next := base.detach()
		nf := reflect.ValueOf(&next).Elem().Field(i)
		switch nf.Kind() {
		case reflect.Map:
			nf.SetMapIndex(reflect.ValueOf("k").Convert(nf.Type().Key()), reflect.Zero(nf.Type().Elem()))
		case reflect.Slice:
			nf.Set(reflect.Append(nf, reflect.Zero(nf.Type().Elem())))
		case reflect.String:
			nf.SetString("x")
		case reflect.Int, reflect.Int64:
			nf.SetInt(nf.Int() + 1)
		case reflect.Uint64:
			nf.SetUint(nf.Uint() + 1)
		case reflect.Struct: // time.Time
			nf.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
		default:
			t.Fatalf("State.%s has an unhandled kind %s; extend diffState and this test", field.Name, nf.Kind())
		}
		c := diffState(&base, &next)
		if c.Empty() {
			t.Errorf("diffState ignores State.%s", field.Name)
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if nf.Kind() == reflect.Map && name != "-" && !slices.Equal(c.Maps[name].Upserted, []string{"k"}) {
			t.Errorf("State.%s changes are not reported under %q: %v", field.Name, name, c.Maps)
		}
	}
}

// projectInvestigationStates rewrites every run with equal values; that must
// not look like a change, or every Update would persist every run.
func TestProjectionAloneChangesNothing(t *testing.T) {
	e := testEngine(t)
	for i := 0; i < 3; i++ {
		ticket := fixture(e, fmt.Sprintf("open-%d", i))
		ticket.Description += fmt.Sprint(i)
		receive(t, e, ticket.ID, ticket)
	}
	analyzedTicket(t, e, "analyzed")
	published := e.Store.Snapshot()
	if len(published.Investigations) == 0 {
		t.Fatal("fixture has no investigations")
	}
	next := published.detach()
	projectInvestigationStates(&next)
	if c := diffState(&published, &next); !c.Empty() {
		t.Fatal("projection without state changes was reported as a change", c)
	}
}

func TestChangeGuardCatchesMissedChange(t *testing.T) {
	if !guardPublishedState {
		t.Skip("guard is off while benchmarking")
	}
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	published := e.Store.Snapshot()
	next := published.detach()
	ticket := next.Tickets["T1"]
	ticket.Assignee = "changed"
	next.Tickets["T1"] = ticket
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "missed") {
			t.Fatal("guard accepted a change the diff did not report", r)
		}
	}()
	verifyChanges(&published, &next, StateChanges{})
}

func TestUpdateReportsChangesToGuardAcrossWorkflow(t *testing.T) {
	// The guard cross-checks every Update in the whole suite; this keeps one
	// explicit end-to-end path here as well.
	e := testEngine(t)
	ticket, _ := analyzedTicket(t, e, "flow")
	closed := ticket
	closed.Closed, closed.Status, closed.UpdatedAt = true, "Resolved", e.Now().Add(time.Minute)
	closed.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	receive(t, e, "closed-flow", closed)
	if e.Store.Snapshot().Tickets["flow"].Closed != true {
		t.Fatal("closing the ticket was not persisted")
	}
}
