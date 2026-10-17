package esa

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
}

func initialState() State {
	return State{SchemaVersion: 1}
}
