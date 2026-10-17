package esa

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// coldSnapshotEngine stores a closed ticket, then reopens the schema in a new
// writer so that ticket's snapshot (referenced by no run or link) stays cold.
func coldSnapshotEngine(t *testing.T) (*Engine, *PostgresPersister, TicketSnapshot) {
	t.Helper()
	e, first, url, schema := pgEngine(t)
	closed := fixture(e, "closed")
	closed.Closed, closed.Status = true, "Resolved"
	closed.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	receive(t, e, "closed-event", closed)
	snap := e.Store.Snapshot().Snapshots.Get(e.Store.Snapshot().LatestSnapshot.Get("closed"))
	first.Close()
	p := openTestPostgres(t, url, schema)
	reopened := testEngine(t)
	reopened.Store = openTestPostgresStore(t, p)
	if _, inMemory := reopened.Store.Snapshot().Snapshots.Lookup(snap.ID); inMemory {
		t.Fatal("fixture snapshot is not cold")
	}
	return reopened, p, snap
}

func TestColdSnapshotIsServedFromStorage(t *testing.T) {
	e, _, snap := coldSnapshotEngine(t)
	w := classifierRequest(e, "GET", "/v1/snapshots/"+snap.ID, "", "1", "closed")
	want, _ := json.Marshal(snap)
	if w.Code != 200 || jsonCompact(t, w.Body.Bytes()) != string(want) {
		t.Fatal("cold snapshot not served as stored", w.Code, w.Body.String())
	}
	if _, inMemory := e.Store.Snapshot().Snapshots.Lookup(snap.ID); inMemory {
		t.Fatal("a cold read loaded the snapshot into memory")
	}
	for _, c := range []struct{ name, path, scope, tickets string }{
		{"other project", "/v1/snapshots/" + snap.ID, "2", "closed"},
		{"other ticket", "/v1/snapshots/" + snap.ID, "1", "other"},
		{"unknown snapshot", "/v1/snapshots/snapshot_missing", "1", "closed"},
	} {
		if w := classifierRequest(e, "GET", c.path, "", c.scope, c.tickets); w.Code != 404 {
			t.Error("cold read ignored visibility:", c.name, w.Code)
		}
	}
	// The current ticket becoming private hides its earlier snapshots too.
	private := fixture(e, "closed")
	private.Closed, private.Status, private.Private = true, "Resolved", true
	private.UpdatedAt = private.UpdatedAt.Add(time.Minute)
	receive(t, e, "private-event", private)
	if w := classifierRequest(e, "GET", "/v1/snapshots/"+snap.ID, "", "1", "closed"); w.Code != 404 {
		t.Fatal("cold snapshot of a now-private ticket was served", w.Code)
	}
}

func jsonCompact(t *testing.T, b []byte) string {
	t.Helper()
	var v TicketSnapshot
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func TestColdSnapshotReadsRunAlongsideWrites(t *testing.T) {
	e, _, snap := coldSnapshotEngine(t)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if w := classifierRequest(e, "GET", "/v1/snapshots/"+snap.ID, "", "1", "closed"); w.Code != 200 {
					errs <- fmt.Errorf("cold read returned %d", w.Code)
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		receive(t, e, fmt.Sprintf("write-%d", i), fixture(e, fmt.Sprintf("T%d", i)))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestColdSnapshotStorageFailureIsUnavailable(t *testing.T) {
	e, p, snap := coldSnapshotEngine(t)
	if w := classifierRequest(e, "GET", "/v1/snapshots/"+snap.ID, "", "1", "closed"); w.Code != 200 {
		t.Fatal("cold read failed before the outage", w.Code)
	}
	// Drop the read session and make reconnecting fail.
	p.readMu.Lock()
	p.reads.Close(context.Background())
	p.readMu.Unlock()
	p.url = "postgres://esa@127.0.0.1:1/unreachable?connect_timeout=1"
	if w := classifierRequest(e, "GET", "/v1/snapshots/"+snap.ID, "", "1", "closed"); w.Code != 503 {
		t.Fatal("storage outage was not reported as unavailable", w.Code, w.Body.String())
	}
}
