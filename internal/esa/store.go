package esa

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store serializes detached-state transactions over JSON or one authoritative backend.
// Network analysis and other irreversible side effects must stay outside Update callbacks.
type Store struct {
	mu       sync.RWMutex
	path     string
	state    State
	backend  StateBackend
	revision uint64
	// guardBytes is the encoded published state, kept only while
	// guardPublishedState is on.
	guardBytes []byte
	wakeMu     sync.Mutex
	wake       chan struct{}
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
	var state State
	var revision uint64
	var err error
	if backend, ok := s.backend.(interface {
		LoadIfChanged(uint64) (State, uint64, bool, error)
	}); ok {
		var changed bool
		state, revision, changed, err = backend.LoadIfChanged(s.revision)
		if err != nil || !changed {
			return err
		}
	} else {
		state, revision, err = s.backend.Load()
	}
	if err != nil {
		return err
	}
	if err = validateState(state); err != nil {
		return err
	}
	if revision >= s.revision {
		s.state, s.revision = state, revision
		s.publishLocked()
	}
	return nil
}

// OpenBackendStore loads and validates the remote authority without a JSON fallback.
func OpenBackendStore(backend StateBackend) (*Store, error) {
	state, revision, err := backend.Load()
	if err != nil {
		return nil, err
	}
	if err = validateState(state); err != nil {
		return nil, err
	}
	s := &Store{state: state, backend: backend, revision: revision}
	s.publishLocked()
	return s, nil
}
func validateState(s State) error {
	if s.SchemaVersion != 1 || s.Tickets == nil || s.Events == nil || s.Receipts == nil || s.Runs == nil || s.Latest == nil || s.Cases == nil {
		return errors.New("invalid store schema")
	}
	return nil
}
func (s *Store) Refresh() error {
	if s.backend == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked()
}

// OpenStore opens a JSON state frame; an empty path selects an in-memory store.
// The service launcher, not this function, takes the cross-process writer lock.
func OpenStore(path string) (*Store, error) {
	s, err := openStore(path)
	if err == nil {
		s.publishLocked()
	}
	return s, err
}
func openStore(path string) (*Store, error) {
	s := &Store{path: path, state: State{SchemaVersion: 1, Tickets: map[string]Ticket{}, Events: map[string]Event{}, Receipts: map[string]Receipt{}, Runs: map[string]Run{}, Latest: map[string]string{}, Cases: map[string]Case{}}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &s.state); err != nil {
		return nil, err
	}
	if s.state.SchemaVersion != 1 || s.state.Tickets == nil || s.state.Events == nil || s.state.Receipts == nil || s.state.Runs == nil || s.state.Latest == nil || s.state.Cases == nil {
		return nil, errors.New("invalid store schema")
	}
	return s, nil
}

// Update commits a detached state atomically. Failed writes never alter memory.
// Update callbacks may run again after a remote CAS conflict.
// Keep callbacks repeatable and free of provider calls or external side effects.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt < 64; attempt++ {
		if s.backend != nil {
			if err := s.refreshLocked(); err != nil {
				return err
			}
		}
		err := s.updateLocked(fn)
		if s.backend != nil && errors.Is(err, errStateCAS) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: state contention; retry the idempotent request", ErrConflict)
}
func (s *Store) updateLocked(fn func(*State) error) error {
	s.verifyPublishedLocked()
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	var next State
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	if next.Snapshots != nil || next.Investigations != nil {
		prepareInvestigations(&next)
	}
	err = fn(&next)
	s.verifyPublishedLocked()
	if err != nil {
		return err
	}
	projectInvestigationStates(&next)
	updated, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if bytes.Equal(b, updated) {
		return nil
	}
	if s.backend != nil {
		revision, err := s.backend.Commit(next, s.revision)
		if err != nil {
			return err
		}
		s.revision = revision
	}
	if s.path != "" {
		b, err = json.Marshal(next)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(s.path), ".esa-state-*")
		if err != nil {
			return err
		}
		name := f.Name()
		defer os.Remove(name)
		if err = f.Chmod(0600); err == nil {
			_, err = f.Write(b)
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err = os.Rename(name, s.path); err != nil {
			return err
		}
		dir, err := os.Open(filepath.Dir(s.path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return err
		}
	}
	s.state = next
	s.publishLocked()
	return nil
}

// guardPublishedState makes in-place mutation of published state fail loudly.
// Tests turn it on (TestMain); it re-encodes the whole state per operation.
var guardPublishedState bool

// publishLocked records the newly published state. Callers hold s.mu for
// writing, or own s exclusively while constructing it.
func (s *Store) publishLocked() {
	if guardPublishedState {
		s.guardBytes, _ = json.Marshal(s.state)
	}
}

// verifyPublishedLocked panics if anyone changed the published state in place
// since it was published, through a shared map, slice or nested value.
func (s *Store) verifyPublishedLocked() {
	if !guardPublishedState {
		return
	}
	if b, _ := json.Marshal(s.state); !bytes.Equal(b, s.guardBytes) {
		panic("esa: published state was mutated in place; Update callbacks and Snapshot readers must replace values, never edit shared ones")
	}
}

// verifyPublished runs the published-state guard; tests call it when they end.
func (s *Store) verifyPublished() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.verifyPublishedLocked()
}

// Snapshot returns a detached copy so callers cannot mutate authoritative state.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.verifyPublishedLocked()
	b, _ := json.Marshal(s.state)
	var snapshot State
	_ = json.Unmarshal(b, &snapshot)
	return snapshot
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
