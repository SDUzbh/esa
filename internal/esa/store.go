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

type Store struct {
	mu       sync.RWMutex
	path     string
	state    State
	backend  StateBackend
	revision uint64
}

// StateBackend provides authoritative, transactional persistence. JSON remains
// the single-process compatibility adapter; remote commits use optimistic CAS.
type StateBackend interface {
	Load() (State, uint64, error)
	Commit(State, uint64) (uint64, error)
}

func OpenBackendStore(backend StateBackend) (*Store, error) {
	state, revision, err := backend.Load()
	if err != nil {
		return nil, err
	}
	if err = validateState(state); err != nil {
		return nil, err
	}
	return &Store{state: state, backend: backend, revision: revision}, nil
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
	state, revision, err := s.backend.Load()
	if err != nil {
		return err
	}
	if err = validateState(state); err != nil {
		return err
	}
	if revision >= s.revision {
		s.state = state
		s.revision = revision
	}
	return nil
}

func OpenStore(path string) (*Store, error) {
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
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt < 64; attempt++ {
		if s.backend != nil {
			state, revision, err := s.backend.Load()
			if err != nil {
				return err
			}
			if err = validateState(state); err != nil {
				return err
			}
			s.state = state
			s.revision = revision
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
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	var next State
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	if next.Snapshots != nil || next.Investigations != nil {
		prepareV2(&next)
	}
	if err = fn(&next); err != nil {
		return err
	}
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
	return nil
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(s.state)
	var snapshot State
	_ = json.Unmarshal(b, &snapshot)
	return snapshot
}

// Polling an idle worker must not copy and JSON-decode the entire archive.
func (s *Store) HasQueuedRun() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, run := range s.state.Runs {
		if run.State == "queued" {
			return true
		}
	}
	return false
}

func (s *Store) needsSharedWork(now time.Time, provider bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, task := range s.state.Investigations {
		if task.State == "queued" || task.State == "running" && !task.LeaseUntil.After(now) || provider && task.State == "awaiting_configuration" && !task.InvocationStarted {
			return true
		}
	}
	for _, run := range s.state.Runs {
		if run.State == "queued" && run.WorkflowVersion != "5" {
			return true
		}
	}
	return false
}
