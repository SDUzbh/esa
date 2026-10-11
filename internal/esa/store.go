package esa

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	mu    sync.RWMutex
	path  string
	state State
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
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	var next State
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	if err = fn(&next); err != nil {
		return err
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
