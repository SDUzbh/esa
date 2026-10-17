package esa

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Persister durably stores the state a Store publishes. Load returns the
// authoritative state and its revision. Apply commits one Update that changed
// something, computed against Commit.Revision, and returns the new revision.
// A failed Apply must leave the stored state as it was; the Store then keeps
// its published state.
type Persister interface {
	Load() (State, uint64, error)
	Apply(Commit) (uint64, error)
}

// Commit is one Update to persist.
type Commit struct {
	// State is the complete next state. Persisters must treat it as read-only.
	State State
	// Changes lists what differs from the published state.
	Changes StateChanges
	// Revision is the published revision the change was computed against.
	Revision uint64
	// Encoded is State as JSON, set only for whole-state persisters.
	Encoded []byte
}

// wholeStatePersister marks persisters that store the whole state as one JSON
// document. The Store encodes the state for them and skips Apply when the
// encoding did not change; other persisters never pay for a whole encoding.
type wholeStatePersister interface {
	Persister
	wholeState()
}

func initialState() State {
	return State{SchemaVersion: 1}
}

// jsonPersister keeps the state in one JSON file, written atomically with
// fsync and a directory sync. An empty path keeps it in memory only. It is a
// single-process adapter: the service launcher takes the writer lock.
type jsonPersister struct {
	path string
}

func (*jsonPersister) wholeState() {}

func (p *jsonPersister) Load() (State, uint64, error) {
	state := initialState()
	if p.path == "" {
		return state, 0, nil
	}
	b, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, 0, nil
	}
	if err != nil {
		return State{}, 0, err
	}
	if err = json.Unmarshal(b, &state); err != nil {
		return State{}, 0, err
	}
	return state, 0, nil
}

func (p *jsonPersister) Apply(c Commit) (uint64, error) {
	if p.path == "" {
		return c.Revision + 1, nil
	}
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, ".esa-state-*")
	if err != nil {
		return 0, err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(c.Encoded)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	if err = os.Rename(name, p.path); err != nil {
		return 0, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return 0, err
	}
	return c.Revision + 1, nil
}
