package esa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTransactionalBackendAcrossWorkers(t *testing.T) {
	seed, _ := OpenStore("")
	state := seed.Snapshot()
	var revision uint64
	var mu sync.Mutex
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 123456789012345678901234" {
			w.WriteHeader(401)
			return
		}
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "PUT" {
			var packet struct {
				State    State  `json:"state"`
				Expected uint64 `json:"expected_revision"`
			}
			if json.NewDecoder(r.Body).Decode(&packet) != nil {
				w.WriteHeader(400)
				return
			}
			if packet.Expected != revision {
				w.WriteHeader(409)
				return
			}
			state = packet.State
			revision++
		}
		_ = json.NewEncoder(w).Encode(statePacket{State: state, Revision: revision})
	}))
	defer server.Close()
	backend := HTTPStateBackend{URL: server.URL, Token: "123456789012345678901234", Client: server.Client()}
	first, err := OpenBackendStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenBackendStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	base := testEngine(t)
	cfg := base.Config
	cfg.Worker.LeaseSeconds = 30
	a, err := NewEngine(first, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewEngine(second, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Now = base.Now
	b.Now = a.Now
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			engine := a
			if i%2 == 1 {
				engine = b
			}
			ticket := fixture(engine, fmt.Sprintf("remote-%d", i))
			if _, err := engine.Receive(Event{ID: ticket.ID, Origin: "test", ForwardedBy: "human", ForwardedAt: engine.Now(), Ticket: ticket}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	var calls atomic.Int32
	analyzer := analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		calls.Add(1)
		return diagnosticReport(), nil
	})
	wg.Add(2)
	for _, engine := range []*Engine{a, b} {
		go func(engine *Engine) {
			defer wg.Done()
			if _, err := engine.ProcessOne(context.Background(), analyzer); err != nil {
				t.Error(err)
			}
		}(engine)
	}
	wg.Wait()
	if err := first.Refresh(); err != nil {
		t.Fatal(err)
	}
	s := first.Snapshot()
	if calls.Load() != 1 || len(s.Investigations) != 1 || len(s.Snapshots) != 20 {
		t.Fatalf("calls=%d tasks=%d snapshots=%d", calls.Load(), len(s.Investigations), len(s.Snapshots))
	}
	before := hash(s)
	unavailable.Store(true)
	if err := first.Update(func(s *State) error { s.Generation++; return nil }); err == nil {
		t.Fatal("unavailable authority accepted a write")
	}
	if hash(first.Snapshot()) != before {
		t.Fatal("failed transaction changed local state")
	}
}
