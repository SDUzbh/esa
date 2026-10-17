package esa

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// recordingPersister records commits.
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
	p := &recordingPersister{initial: initialState(), revision: 7}
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
	if c.Revision != 7 || !slices.Equal(c.Changes.Maps["tickets"].Upserted, []string{"T1"}) || c.State.Tickets.Get("T1").ID != "T1" {
		t.Fatal("unexpected commit", c.Revision, c.Changes)
	}
	if s.revision != 8 {
		t.Fatal("store did not adopt the persister's revision", s.revision)
	}
}

func TestStoreFailedApplyLeavesMemoryUnchanged(t *testing.T) {
	e := testEngine(t)
	p := &recordingPersister{initial: initialState(), fail: errors.New("disk full")}
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

func TestReadLegacyStoreChecksTheFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadLegacyStore(filepath.Join(dir, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a missing legacy store was not reported", err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLegacyStore(path); err == nil {
		t.Fatal("invalid schema accepted")
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLegacyStore(path); err == nil {
		t.Fatal("a store without its core tables was accepted")
	}
}
