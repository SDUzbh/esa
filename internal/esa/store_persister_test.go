package esa

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// recordingPersister records commits; it does not store whole states, so the
// Store must not encode one for it.
type recordingPersister struct {
	initial  State
	revision uint64
	commits  []Commit
	fail     error
}

func (p *recordingPersister) Load() (State, uint64, error) { return p.initial, p.revision, nil }
func (p *recordingPersister) Apply(c Commit) (uint64, error) {
	p.commits = append(p.commits, c)
	if p.fail != nil {
		return 0, p.fail
	}
	p.revision = c.Revision + 1
	return p.revision, nil
}

// wholeRecorder is a recordingPersister that writes whole states.
type wholeRecorder struct{ recordingPersister }

func (*wholeRecorder) wholeState() {}

func emptyState() State {
	s, _ := OpenStore("")
	return s.Snapshot()
}

func recordingStore(t *testing.T, p Persister) *Store {
	t.Helper()
	s, err := newStore(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.verifyPublished)
	return s
}

func TestStoreAppliesOnlyChangedUpdates(t *testing.T) {
	e := testEngine(t)
	p := &recordingPersister{initial: emptyState(), revision: 7}
	s := recordingStore(t, p)
	if err := s.Update(func(*State) error { return nil }); err != nil || len(p.commits) != 0 {
		t.Fatal("no-op Update reached the persister", err, len(p.commits))
	}
	if err := s.Update(func(st *State) error { st.Tickets.Put("T1", fixture(e, "T1")); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(p.commits) != 1 {
		t.Fatal("changed Update was not applied once", len(p.commits))
	}
	c := p.commits[0]
	if c.Revision != 7 || c.Encoded != nil || !slices.Equal(c.Changes.Maps["tickets"].Upserted, []string{"T1"}) || c.State.Tickets.Get("T1").ID != "T1" {
		t.Fatal("unexpected commit", c.Revision, c.Encoded != nil, c.Changes)
	}
	if s.revision != 8 {
		t.Fatal("store did not adopt the persister's revision", s.revision)
	}
}

func TestStoreFailedApplyLeavesMemoryUnchanged(t *testing.T) {
	e := testEngine(t)
	p := &recordingPersister{initial: emptyState(), fail: errors.New("disk full")}
	s := recordingStore(t, p)
	err := s.Update(func(st *State) error { st.Tickets.Put("T1", fixture(e, "T1")); return nil })
	if err == nil || s.Snapshot().Tickets.Len() != 0 || s.revision != 0 {
		t.Fatal("failed Apply changed memory", err, s.Snapshot().Tickets.Len(), s.revision)
	}
	p.fail = nil
	if err = s.Update(func(st *State) error { st.Tickets.Put("T1", fixture(e, "T1")); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := p.commits[1].Changes.Maps["tickets"].Upserted; !slices.Equal(got, []string{"T1"}) {
		t.Fatal("retry did not diff against the unchanged state", got)
	}
}

func TestWholeStatePersisterGetsEncodingAndSkipsEqualContent(t *testing.T) {
	e := testEngine(t)
	initial := emptyState()
	initial.Tickets = TableOf(map[string]Ticket{"T1": fixture(e, "T1")})
	p := &wholeRecorder{recordingPersister{initial: initial}}
	s := recordingStore(t, p)
	// Rebuilding a slice with equal content changes pointers but not the encoding.
	err := s.Update(func(st *State) error {
		ticket := st.Tickets.Get("T1")
		ticket.Components = append([]string(nil), ticket.Components...)
		st.Tickets.Put("T1", ticket)
		return nil
	})
	if err != nil || len(p.commits) != 0 {
		t.Fatal("equal-content Update was written", err, len(p.commits))
	}
	if err = s.Update(func(st *State) error { st.Tickets.Put("T2", fixture(e, "T2")); return nil }); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(s.Snapshot())
	if len(p.commits) != 1 || string(p.commits[0].Encoded) != string(want) {
		t.Fatal("whole-state persister did not receive the encoded state")
	}
}

func TestJSONPersisterRoundTripAndSchemaCheck(t *testing.T) {
	e := testEngine(t)
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenStore(path)
	if err != nil || s.Snapshot().Tickets.Len() != 0 {
		t.Fatal("missing file must open an empty store", err)
	}
	if err = s.Update(func(st *State) error { st.Tickets.Put("T1", fixture(e, "T1")); return nil }); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil || reopened.Snapshot().Tickets.Get("T1").ID != "T1" {
		t.Fatal("JSON persister lost a committed Update", err)
	}
	if err = os.WriteFile(path, []byte(`{"schema_version":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenStore(path); err == nil {
		t.Fatal("invalid schema accepted")
	}
}
