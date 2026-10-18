package esa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"unsafe"
)

// MapChanges lists the keys an Update wrote or removed in one State map.
type MapChanges struct {
	Upserted []string
	Deleted  []string
}

func (c MapChanges) empty() bool { return len(c.Upserted) == 0 && len(c.Deleted) == 0 }

// HistoryChange describes an append-only history slice after an Update:
// Appended new elements at the end, or Rewritten when earlier ones changed.
type HistoryChange struct {
	Appended  int
	Rewritten bool
}

// StateChanges is what an Update changed relative to the published state.
// Maps is keyed by each map's JSON field name; Frame covers the scalar fields
// and StoredFields. A persister needs to write only these.
type StateChanges struct {
	Maps              map[string]MapChanges
	CaseHistory       HistoryChange
	MembershipHistory HistoryChange
	Frame             bool
}

// Empty reports whether the Update changed nothing at all.
func (c StateChanges) Empty() bool {
	return len(c.Maps) == 0 && c.CaseHistory == (HistoryChange{}) && c.MembershipHistory == (HistoryChange{}) && !c.Frame
}

func (c *StateChanges) add(name string, m MapChanges) {
	if m.empty() {
		return
	}
	if c.Maps == nil {
		c.Maps = map[string]MapChanges{}
	}
	c.Maps[name] = m
}

// sameBits compares the raw memory of two values. Update shares untouched
// entities with the published state, so an entry the callback did not replace
// is bit-for-bit identical; a replaced entry with equal content may differ in
// pointers or padding, which only causes a harmless extra write.
func sameBits[V any](a, b *V) bool {
	n := unsafe.Sizeof(*a)
	return bytes.Equal(unsafe.Slice((*byte)(unsafe.Pointer(a)), n), unsafe.Slice((*byte)(unsafe.Pointer(b)), n))
}

func diffHistory[V any](old, next []V) HistoryChange {
	if len(next) < len(old) {
		return HistoryChange{Rewritten: true}
	}
	// Appending in place keeps the backing array: the prefix is the same
	// memory and unchanged by construction (the guard rejects in-place edits).
	if len(old) == 0 || &old[0] == &next[0] {
		return HistoryChange{Appended: len(next) - len(old)}
	}
	for i := range old {
		if !sameBits(&old[i], &next[i]) {
			return HistoryChange{Rewritten: true}
		}
	}
	return HistoryChange{Appended: len(next) - len(old)}
}

// frame holds the scalar State fields persisted outside the entity maps.
type frame struct {
	IssueAccessSequence uint64
	ReplayClock         [unsafe.Sizeof(State{}.ReplayClock)]byte
	LastClaimedProject  string
	SchemaVersion       int
}

func frameOf(s *State) frame {
	f := frame{IssueAccessSequence: s.IssueAccessSequence, LastClaimedProject: s.LastClaimedProject, SchemaVersion: s.SchemaVersion}
	f.ReplayClock = *(*[unsafe.Sizeof(State{}.ReplayClock)]byte)(unsafe.Pointer(&s.ReplayClock))
	return f
}

// diffState lists every change from old (published) to next (detached and
// edited). TestDiffStateCoversEveryField fails if a State field is missing here.
func diffState(old, next *State) StateChanges {
	var c StateChanges
	c.add("latest_snapshot", diffTable(old.LatestSnapshot, next.LatestSnapshot))
	c.add("snapshots", diffTable(old.Snapshots, next.Snapshots))
	c.add("engineering_issues", diffTable(old.Issues, next.Issues))
	c.add("issue_ticket_links", diffTable(old.Memberships, next.Memberships))
	c.add("issue_overrides", diffTable(old.IssueOverrides, next.IssueOverrides))
	c.add("investigations", diffTable(old.Investigations, next.Investigations))
	c.add("investigation_groups", diffTable(old.Groups, next.Groups))
	c.add("solution_revisions", diffTable(old.Solutions, next.Solutions))
	c.add("classifier_predictions", diffTable(old.ClassifierPredictions, next.ClassifierPredictions))
	c.add("classifier_feedback", diffTable(old.ClassifierFeedback, next.ClassifierFeedback))
	c.add("tickets", diffTable(old.Tickets, next.Tickets))
	c.add("events", diffTable(old.Events, next.Events))
	c.add("receipts", diffTable(old.Receipts, next.Receipts))
	c.add("event_rejections", diffTable(old.Rejections, next.Rejections))
	c.add("runs", diffTable(old.Runs, next.Runs))
	c.add("latest", diffTable(old.Latest, next.Latest))
	c.add("cases", diffTable(old.Cases, next.Cases))
	c.CaseHistory = diffHistory(old.CaseHistory, next.CaseHistory)
	c.MembershipHistory = diffHistory(old.MembershipHistory, next.MembershipHistory)
	oldFrame, nextFrame := frameOf(old), frameOf(next)
	c.Frame = !sameBits(&oldFrame, &nextFrame) || !maps.EqualFunc(
		old.StoredFields,
		next.StoredFields,
		func(a, b json.RawMessage) bool { return bytes.Equal(a, b) },
	)
	return c
}

// verifyProjection is the guard's check that the incremental projection
// matches projecting every investigation. It runs only while
// guardPublishedState is on.
func verifyProjection(next *State) {
	if !guardPublishedState {
		return
	}
	full := next.detach()
	projectInvestigationStates(&full)
	if c := diffTable(next.Runs, full.Runs); len(c.Upserted) > 0 {
		panic(fmt.Sprintf("esa: incremental projection missed runs %v", c.Upserted))
	}
}

// verifyChanges is the guard's cross-check: it re-derives every change by
// comparing JSON encodings and panics if diffState missed one, which would
// mean a lost write once only changes are persisted. Reported changes that
// turn out equal are allowed. It runs only while guardPublishedState is on.
func verifyChanges(old, next *State, c StateChanges) {
	if !guardPublishedState {
		return
	}
	encode := func(v reflect.Value) string { b, _ := json.Marshal(v.Interface()); return string(b) }
	ov, nv := reflect.ValueOf(old).Elem(), reflect.ValueOf(next).Elem()
	for i := 0; i < ov.NumField(); i++ {
		field := ov.Type().Field(i)
		if !field.IsExported() {
			continue // derived indexes, never persisted
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		of, nf := ov.Field(i), nv.Field(i)
		missed := func(what string) {
			panic(fmt.Sprintf("esa: change detection missed State.%s%s; it would not be persisted", field.Name, what))
		}
		switch {
		case of.Addr().Type().Implements(reflect.TypeFor[stateTable]()):
			ot, nt := of.Addr().Interface().(stateTable), nf.Addr().Interface().(stateTable)
			reported := map[string]bool{}
			for _, k := range c.Maps[name].Upserted {
				reported[k] = true
			}
			for _, k := range c.Maps[name].Deleted {
				reported[k] = true
			}
			keys := map[string]bool{}
			for _, k := range ot.tableKeys() {
				keys[k] = true
			}
			for _, k := range nt.tableKeys() {
				keys[k] = true
			}
			for k := range keys {
				a, okA := ot.tableEncode(k)
				b, okB := nt.tableEncode(k)
				if (okA != okB || a != b) && !reported[k] {
					missed("[" + k + "]")
				}
			}
		case of.Kind() == reflect.Slice:
			history := map[string]HistoryChange{"CaseHistory": c.CaseHistory, "MembershipHistory": c.MembershipHistory}[field.Name]
			if history.Rewritten {
				continue
			}
			if nf.Len() < of.Len() {
				missed(" (shrunk)")
			}
			for j := 0; j < of.Len(); j++ {
				if encode(of.Index(j)) != encode(nf.Index(j)) {
					missed(fmt.Sprintf("[%d]", j))
				}
			}
			if nf.Len()-of.Len() != history.Appended {
				missed(" (appended elements)")
			}
		default:
			if encode(of) != encode(nf) && !c.Frame {
				missed("")
			}
		}
	}
}
