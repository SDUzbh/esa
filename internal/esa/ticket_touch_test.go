package esa

import (
	"encoding/json"
	"sync"
	"testing"
)

// countingBackend is an in-memory authority that counts loads. Store.Update
// reloads before every attempt, so loads reveal whether a request tried to write.
type countingBackend struct {
	mu       sync.Mutex
	state    []byte
	revision uint64
	loads    int
}

func (b *countingBackend) Load() (State, uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loads++
	var s State
	err := json.Unmarshal(b.state, &s)
	return s, b.revision, err
}

func (b *countingBackend) Commit(s State, expected uint64) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if expected != b.revision {
		return 0, errStateCAS
	}
	var err error
	b.state, err = json.Marshal(s)
	b.revision++
	return b.revision, err
}

func (b *countingBackend) loadCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loads
}

func backendEngine(t *testing.T) (*Engine, *countingBackend) {
	t.Helper()
	e := testEngine(t)
	empty, _ := OpenStore("")
	backend := &countingBackend{}
	backend.state, _ = json.Marshal(empty.Snapshot())
	store, err := OpenBackendStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	e.Store = store
	t.Cleanup(store.verifyPublished)
	return e, backend
}

// GET performs one Refresh per request; an access-order write adds another load.
func getTicketLoads(t *testing.T, e *Engine, backend *countingBackend, id string) int {
	t.Helper()
	before := backend.loadCount()
	if w := classifierRequest(e, "GET", "/v1/tickets/"+id, "", "1", id); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	return backend.loadCount() - before
}

func TestTicketGETSkipsWriteWhenNoIssueToTouch(t *testing.T) {
	e, backend := backendEngine(t)
	receive(t, e, "plain", fixture(e, "plain"))
	if loads := getTicketLoads(t, e, backend, "plain"); loads != 1 {
		t.Fatal("ticket without an Issue still attempted an access-order write", loads)
	}
}

func TestTicketGETStillTouchesItsIssue(t *testing.T) {
	e, backend := backendEngine(t)
	ticket, run := analyzedTicket(t, e, "analyzed")
	if run.IssueID == "" {
		t.Fatal("fixture has no Issue")
	}
	before := e.Store.Snapshot().Issues[run.IssueID].AccessOrder
	if loads := getTicketLoads(t, e, backend, ticket.ID); loads != 2 {
		t.Fatal("ticket with an Issue did not write its access order", loads)
	}
	if after := e.Store.Snapshot().Issues[run.IssueID].AccessOrder; after <= before {
		t.Fatal("Issue access order was not advanced", before, after)
	}
}
