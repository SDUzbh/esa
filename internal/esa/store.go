package esa

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// Store serializes copy-on-write state transactions and hands each change to
// its Persister (a JSON file or an authoritative backend).
// Network analysis and other irreversible side effects must stay outside Update callbacks.
type Store struct {
	mu        sync.RWMutex
	state     State
	persister Persister
	revision  uint64
	// encoded is json.Marshal(state) as last published. It is kept only for
	// whole-state persisters, to skip writes whose encoding did not change, and
	// for the test guard; other persisters never encode the whole state.
	encoded []byte
	wakeMu  sync.Mutex
	wake    chan struct{}
}

// WorkSignal returns a channel that closes the next time a worker or an
// engineering slot frees in this process. Idle workers wait on it together with
// a slow poll, which covers other processes sharing the state and time limits.
func (s *Store) WorkSignal() <-chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{})
	}
	return s.wake
}

func (s *Store) signalWork() {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wake != nil {
		close(s.wake)
		s.wake = nil
	}
}

// StateBackend provides authoritative, transactional persistence. JSON remains
// a local single-process adapter; remote commits use optimistic CAS.
type StateBackend interface {
	Load() (State, uint64, error)
	Commit(State, uint64) (uint64, error)
}

func (s *Store) refreshLocked() error {
	r, ok := s.persister.(refresher)
	if !ok {
		return nil
	}
	state, revision, changed, err := r.refresh(s.revision)
	if err != nil || !changed {
		return err
	}
	if err = validateState(state); err != nil {
		return err
	}
	if revision >= s.revision {
		s.state, s.revision = state, revision
		s.publishLocked(nil)
	}
	return nil
}

// OpenBackendStore loads and validates the remote authority without a JSON fallback.
func OpenBackendStore(backend StateBackend) (*Store, error) {
	return newStore(&backendPersister{backend: backend})
}

// newStore loads and validates the persister's state and publishes it.
func newStore(p Persister) (*Store, error) {
	state, revision, err := p.Load()
	if err != nil {
		return nil, err
	}
	if err = validateState(state); err != nil {
		return nil, err
	}
	s := &Store{state: state, persister: p, revision: revision}
	s.publishLocked(nil)
	return s, nil
}
func validateState(s State) error {
	if s.SchemaVersion != 1 || s.Tickets == nil || s.Events == nil || s.Receipts == nil || s.Runs == nil || s.Latest == nil || s.Cases == nil {
		return errors.New("invalid store schema")
	}
	return nil
}
func (s *Store) Refresh() error {
	if _, ok := s.persister.(refresher); !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked()
}

// OpenStore opens a JSON state frame; an empty path selects an in-memory store.
// The service launcher, not this function, takes the cross-process writer lock.
func OpenStore(path string) (*Store, error) {
	return newStore(&jsonPersister{path: path})
}

// Update commits a copy-on-write state atomically. Failed writes never alter memory.
// The callback gets new top-level maps and slices that share unchanged entities:
// replace an entity (s.Tickets[id] = t) and never edit a shared map, slice or
// nested value in place.
// Update callbacks may run again after a remote CAS conflict.
// Keep callbacks repeatable and free of provider calls or external side effects.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, remote := s.persister.(refresher)
	for attempt := 0; attempt < 64; attempt++ {
		if remote {
			if err := s.refreshLocked(); err != nil {
				return err
			}
		}
		err := s.updateLocked(fn)
		if remote && errors.Is(err, errStateCAS) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: state contention; retry the idempotent request", ErrConflict)
}
func (s *Store) updateLocked(fn func(*State) error) error {
	s.verifyPublishedLocked()
	next := s.state.detach()
	if next.Snapshots != nil || next.Investigations != nil {
		prepareInvestigations(&next)
	}
	err := fn(&next)
	s.verifyPublishedLocked()
	if err != nil {
		return err
	}
	projectInvestigationStates(&next)
	// Only changed entities differ from the published state; when nothing
	// changed, skip encoding the whole state. In tests the guard re-derives
	// every change from JSON encodings and panics if the diff missed one.
	changes := diffState(&s.state, &next)
	verifyChanges(&s.state, &next, changes)
	if changes.Empty() {
		return nil
	}
	commit := Commit{State: next, Changes: changes, Revision: s.revision}
	if _, whole := s.persister.(wholeStatePersister); whole {
		if commit.Encoded, err = json.Marshal(next); err != nil {
			return err
		}
		// A replaced entity with equal content is reported as changed; keep
		// skipping writes whose encoding is unchanged, exactly as before.
		if bytes.Equal(s.encoded, commit.Encoded) {
			return nil
		}
	}
	revision, err := s.persister.Apply(commit)
	if err != nil {
		return err
	}
	s.state, s.revision = next, revision
	s.publishLocked(commit.Encoded)
	return nil
}

// guardPublishedState makes in-place mutation of published state fail loudly.
// Update shares unchanged entities with the published state (copy-on-write),
// so an in-place edit would also corrupt s.encoded and later change detection.
// Tests turn it on (TestMain); it re-encodes the whole state per operation.
var guardPublishedState bool

// publishLocked records the encoding of the newly published state when a
// whole-state persister or the test guard needs it; encoded may be passed in
// when the caller already has it. Callers hold s.mu for writing, or own s
// exclusively while constructing it.
func (s *Store) publishLocked(encoded []byte) {
	_, whole := s.persister.(wholeStatePersister)
	switch {
	case encoded != nil:
		s.encoded = encoded
	case whole || guardPublishedState:
		s.encoded, _ = json.Marshal(s.state)
	default:
		s.encoded = nil
	}
}

// verifyPublishedLocked panics if anyone changed the published state in place
// since it was published, through a shared map, slice or nested value.
func (s *Store) verifyPublishedLocked() {
	if !guardPublishedState {
		return
	}
	if b, _ := json.Marshal(s.state); !bytes.Equal(b, s.encoded) {
		panic("esa: published state was mutated in place; Update callbacks and Snapshot readers must replace values, never edit shared ones")
	}
}

// verifyPublished runs the published-state guard; tests call it when they end.
func (s *Store) verifyPublished() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.verifyPublishedLocked()
}

// Snapshot returns the published state without copying it. Writers never
// change a published state (Update is copy-on-write), so the result stays
// consistent after the lock is released. It is read-only: callers must not
// edit its maps, slices or entities; use Clone for a state to modify.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.verifyPublishedLocked()
	return s.state
}

// Clone returns a fully independent copy of the published state.
func (s *Store) Clone() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(s.state)
	var clone State
	_ = json.Unmarshal(b, &clone)
	return clone
}

// detach returns a state whose top-level maps and slices can be changed without
// touching s. Entities themselves are shared and must be replaced, not edited.
// Slices are clipped so an append always reallocates instead of writing into
// an array that the published state still references.
func (s State) detach() State {
	next := s
	next.StoredFields = maps.Clone(s.StoredFields)
	next.LatestSnapshot = maps.Clone(s.LatestSnapshot)
	next.Snapshots = maps.Clone(s.Snapshots)
	next.Issues = maps.Clone(s.Issues)
	next.Memberships = maps.Clone(s.Memberships)
	next.MembershipHistory = slices.Clip(s.MembershipHistory)
	next.IssueOverrides = maps.Clone(s.IssueOverrides)
	next.Investigations = maps.Clone(s.Investigations)
	next.Groups = maps.Clone(s.Groups)
	next.Solutions = maps.Clone(s.Solutions)
	next.ClassifierPredictions = maps.Clone(s.ClassifierPredictions)
	next.ClassifierFeedback = maps.Clone(s.ClassifierFeedback)
	next.Tickets = maps.Clone(s.Tickets)
	next.Events = maps.Clone(s.Events)
	next.Receipts = maps.Clone(s.Receipts)
	next.Rejections = maps.Clone(s.Rejections)
	next.Runs = maps.Clone(s.Runs)
	next.Latest = maps.Clone(s.Latest)
	next.Cases = maps.Clone(s.Cases)
	next.CaseHistory = slices.Clip(s.CaseHistory)
	return next
}

// An idle worker does not copy the archive just to inspect its queue.
// Parked tasks need a worker once a slot frees or their queue wait expires.
func (s *Store) needsWork(now time.Time, provider bool, maxQueueWait time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	free := engineeringActive(s.state) < EngineeringConcurrency
	for _, task := range s.state.Investigations {
		if parked(task) {
			if provider && free || time.Since(task.WaitingSince) >= maxQueueWait {
				return true
			}
			continue
		}
		if task.State == "queued" || activeInvestigation(task) && !task.LeaseUntil.After(now) || provider && task.State == "awaiting_configuration" && !task.InvocationStarted {
			return true
		}
	}
	for _, run := range s.state.Runs {
		if run.State == "queued" && (run.WorkflowVersion != WorkflowVersion || run.Input.ForwardedBy == "" || run.Input.ForwardedAt.IsZero()) {
			return true
		}
	}
	return false
}
