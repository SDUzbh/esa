package esa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// Store serializes copy-on-write state transactions and hands each change to
// its Persister, Postgres in production.
// Network analysis and other irreversible side effects must stay outside Update callbacks.
type Store struct {
	mu        sync.RWMutex
	state     State
	persister Persister
	revision  uint64
	// encoded is json.Marshal(state) as last published, kept only for the
	// test guard.
	encoded []byte
	// lost runs once when a commit fails with ErrLostWriter.
	lost func(error)
	// updates counts Update calls; tests use it to prove a path did not write.
	updates atomic.Uint64
	wakeMu  sync.Mutex
	wake    chan struct{}
}

// coldSnapshotReader is implemented by persisters that keep some snapshots
// out of memory.
type coldSnapshotReader interface {
	ReadSnapshot(ctx context.Context, id string) (TicketSnapshot, bool, error)
}

// LookupSnapshot returns a snapshot from memory or, when the persister keeps
// unreferenced snapshots out of memory, from storage. It never loads the
// snapshot into memory and is safe to call outside Update.
func (s *Store) LookupSnapshot(ctx context.Context, id string) (TicketSnapshot, bool, error) {
	if snap, ok := s.Snapshot().Snapshots.Lookup(id); ok {
		return snap, true, nil
	}
	if r, ok := s.persister.(coldSnapshotReader); ok {
		return r.ReadSnapshot(ctx, id)
	}
	return TicketSnapshot{}, false, nil
}

// OnLostWriter registers fn to run once, in its own goroutine, when a commit
// fails because another process became the state writer.
func (s *Store) OnLostWriter(fn func(error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lost = fn
}

// WorkSignal returns a channel that closes the next time a worker or an
// engineering slot frees in this process. Idle workers wait on it together with
// a slow poll, which covers queue-wait limits and lease expiry.
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

// newStore loads and validates the persister's state and publishes it.
func newStore(p Persister) (*Store, error) {
	state, revision, err := p.Load()
	if err != nil {
		return nil, err
	}
	if err = validateState(state); err != nil {
		return nil, err
	}
	state.advanceIndexes()
	s := &Store{state: state, persister: p, revision: revision}
	s.publishLocked()
	return s, nil
}

// validateState checks the schema version. A JSON frame missing a core table
// is rejected while decoding (State.UnmarshalJSON), since an empty Table and
// an absent one look the same once decoded.
func validateState(s State) error {
	if s.SchemaVersion != 1 {
		return errors.New("invalid store schema")
	}
	return nil
}

// Update commits a copy-on-write state atomically. Failed writes never alter memory.
// The callback gets new top-level maps and slices that share unchanged entities:
// replace an entity (s.Tickets[id] = t) and never edit a shared map, slice or
// nested value in place.
// Keep callbacks repeatable and free of provider calls or external side effects.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates.Add(1)
	err := s.updateLocked(fn)
	if errors.Is(err, ErrLostWriter) && s.lost != nil {
		go s.lost(err)
		s.lost = nil
	}
	return err
}
func (s *Store) updateLocked(fn func(*State) error) error {
	s.verifyPublishedLocked()
	next := s.state.detach()
	err := fn(&next)
	s.verifyPublishedLocked()
	if err != nil {
		return err
	}
	projectChangedInvestigations(&s.state, &next)
	verifyProjection(&next)
	// Only changed entities differ from the published state; when nothing
	// changed, nothing reaches the persister. In tests the guard re-derives
	// every change from JSON encodings and panics if the diff missed one.
	changes := diffState(&s.state, &next)
	verifyChanges(&s.state, &next, changes)
	verifyTransitions(&s.state, &next, changes)
	if changes.Empty() {
		return nil
	}
	commit := Commit{State: next, Changes: changes, Revision: s.revision}
	revision, err := s.persister.Apply(commit)
	if err != nil {
		return err
	}
	next.advanceIndexes()
	verifyIndexes(&next)
	s.state, s.revision = next, revision
	s.publishLocked()
	return nil
}

// guardPublishedState makes in-place mutation of published state fail loudly.
// Update shares unchanged entities with the published state (copy-on-write),
// so an in-place edit would also corrupt s.encoded and later change detection.
// Tests turn it on (TestMain); it re-encodes the whole state per operation.
var guardPublishedState bool

// publishLocked records the encoding of the newly published state for the
// test guard. Callers hold s.mu for writing, or own s exclusively while
// constructing it.
func (s *Store) publishLocked() {
	s.encoded = nil
	if guardPublishedState {
		s.encoded, _ = json.Marshal(s.state)
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

// detach returns a state that an Update can change without touching s, in
// O(1): Tables are persistent, so the copy shares every node and a write
// copies only its own path. Entities are shared too and must be replaced, not
// edited in place. History slices are append-only: an append may write past
// the published length into the shared array, which no reader sees, since
// every published State carries its own length and only one Update runs at a
// time; elements before that length are never written.
func (s State) detach() State {
	next := s
	next.StoredFields = maps.Clone(s.StoredFields)
	return next
}

// An idle worker does not copy the archive just to inspect its queue.
// Parked tasks need a worker once a slot frees or their queue wait expires.
func (s *Store) needsWork(now time.Time, provider bool, maxQueueWait time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	free := engineeringActive(s.state) < EngineeringConcurrency
	states := []InvestigationState{InvestigationQueued, InvestigationRunning, InvestigationInvestigating}
	if provider {
		states = append(states, InvestigationAwaitingConfiguration)
	}
	for _, task := range s.state.investigationsIn(states...) {
		if parked(task) {
			if provider && free && !providerPaused(s.state) || time.Since(queueWaitStart(s.state, task)) >= maxQueueWait {
				return true
			}
			continue
		}
		if task.State == InvestigationQueued && !(provider && providerPaused(s.state)) ||
			activeInvestigation(task) && !task.LeaseUntil.After(now) ||
			provider && task.State == InvestigationAwaitingConfiguration && !task.InvocationStarted {
			return true
		}
	}
	for _, run := range s.state.queuedRuns() {
		if run.WorkflowVersion != WorkflowVersion || run.Input.ForwardedBy == "" || run.Input.ForwardedAt.IsZero() {
			return true
		}
	}
	return false
}
