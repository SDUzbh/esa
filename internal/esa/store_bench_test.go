package esa

// Store scaling benchmarks (REVIEW_ISSUES.md Issue 1). They are not run by
// go test without -bench. The Memory, Store and TicketGET benchmarks keep the
// state behind a discardPersister, measuring the in-memory work; the Postgres
// one adds the database write. Example:
//
//	go test ./internal/esa -run '^$' -bench . -benchtime 5x
//
// ESA_BENCH_SIZES overrides the ticket counts, e.g. ESA_BENCH_SIZES=1000,10000.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var benchDescription = strings.Repeat("stack trace line at org.apache.kafka.Foo(Foo.java:123)\n", 40)

func benchSizes(b *testing.B) []int {
	sizes := []int{1000, 5000}
	if value := os.Getenv("ESA_BENCH_SIZES"); value != "" {
		sizes = nil
		for _, field := range strings.Split(value, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil || n < 1 {
				b.Fatalf("invalid ESA_BENCH_SIZES %q", value)
			}
			sizes = append(sizes, n)
		}
	}
	return sizes
}

func benchClosedTicket(e *Engine, id string) Ticket {
	ticket := fixture(e, id)
	ticket.Description, ticket.Closed = benchDescription, true
	ticket.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	return ticket
}

// seeded builds each size once per benchmark binary run; Go invokes a
// benchmark function several times while it calibrates b.N.
var seeded sync.Map

func BenchmarkStoreSnapshot(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seedMemoryStore(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = e.Store.Snapshot()
			}
		})
	}
}

func BenchmarkStoreUpdateNoop(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seedMemoryStore(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.Store.Update(func(*State) error { return nil }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTicketGET measures the plugin's single-ticket read end to end.
func BenchmarkTicketGET(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seedMemoryStore(b, n)
			handler := (&API{Engine: e, Token: strings.Repeat("t", 24)}).Handler()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodGet, "/v1/tickets/K-1", nil)
				req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 24))
				req.Header.Set("X-ESA-Project-IDs", "1")
				req.Header.Set("X-ESA-Ticket-IDs", "K-1")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					b.Fatalf("GET returned %d: %s", w.Code, w.Body.String())
				}
			}
		})
	}
}

// BenchmarkPostgresUpdateOneTicket measures a one-ticket write on the Postgres
// store.
func BenchmarkPostgresUpdateOneTicket(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			url := testDatabaseURL(b)
			seed := seedMemoryStore(b, n).Store.Snapshot()
			p := openTestPostgres(b, url, testSchema(b, url))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := p.AcquireWriter(ctx, 20*time.Millisecond); err != nil {
				b.Fatal(err)
			}
			// Seed with the bulk import, as a real migration does.
			if err := p.Import(ctx, seed); err != nil {
				b.Fatal(err)
			}
			store, err := newStore(p)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := store.Update(func(s *State) error {
					ticket := s.Tickets.Get("K-1")
					ticket.Assignee = fmt.Sprint(i)
					s.Tickets.Put(ticket.ID, ticket)
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// discardPersister accepts every commit without storing it, so benchmarks
// measure only the in-memory work of Update (detach, callback, diff).
type discardPersister struct{ initial State }

func (p discardPersister) Load() (State, uint64, error) { return p.initial, 0, nil }
func (discardPersister) Apply(c Commit) (uint64, error) { return c.Revision + 1, nil }

// seedMemoryStore builds n closed tickets directly into a state (no encoding)
// behind a discardPersister.
func seedMemoryStore(b *testing.B, n int) *Engine {
	b.Helper()
	cacheKey := fmt.Sprintf("memory/%d", n)
	if e, ok := seeded.Load(cacheKey); ok {
		return e.(*Engine)
	}
	e := testEngine(b)
	template := benchClosedTicket(e, "K-0")
	receipt, err := e.Receive(Event{ID: "ev-0", Origin: "bench", ForwardedBy: "u", ForwardedAt: e.Now(), Ticket: template})
	if err != nil {
		b.Fatal(err)
	}
	s := e.Store.Snapshot().detach()
	event, snapshot, c := s.Events.Get("ev-0"), s.Snapshots.Get(s.LatestSnapshot.Get("K-0")), s.Cases.Get(receipt.CaseID)
	for i := 1; i < n; i++ {
		ticketID, eventID := fmt.Sprintf("K-%d", i), fmt.Sprintf("ev-%d", i)
		ticket := s.Tickets.Get("K-0")
		ticket.ID, ticket.SourceKey = ticketID, ticketID
		s.Tickets.Put(ticketID, ticket)
		event.ID, event.TicketID, event.TicketHash, event.SnapshotID = eventID, ticketID, hash(ticket), snapshotIDFor(eventID)
		s.Events.Put(eventID, event)
		snapshot.ID, snapshot.EventID, snapshot.Ticket, snapshot.Hash = snapshotIDFor(eventID), eventID, ticket, hash(ticket)
		s.Snapshots.Put(snapshot.ID, snapshot)
		s.LatestSnapshot.Put(ticketID, snapshot.ID)
		c.ID, c.TicketID, c.SourceKey = "case_"+hash(ticketID)[:24], ticketID, ticketID
		s.Cases.Put(c.ID, c)
		s.CaseHistory = append(s.CaseHistory, c)
		s.Receipts.Put(eventID, Receipt{EventID: eventID, TicketID: ticketID, CaseID: c.ID, Action: receipt.Action})
	}
	store, err := newStore(discardPersister{initial: s})
	if err != nil {
		b.Fatal(err)
	}
	e.Store = store
	seeded.Store(cacheKey, e)
	return e
}

// BenchmarkMemoryUpdateOneTicket is the per-write cost without persistence:
// what the Postgres store pays in memory on top of its ~1 ms database write.
func BenchmarkMemoryUpdateOneTicket(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seedMemoryStore(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := e.update(func(s *State) error {
					ticket := s.Tickets.Get("K-1")
					ticket.Assignee = fmt.Sprint(i)
					s.Tickets.Put(ticket.ID, ticket)
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMemoryReceiveClosedTicket is one intake without persistence.
func BenchmarkMemoryReceiveClosedTicket(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seedMemoryStore(b, n)
			start := e.Store.Snapshot().Tickets.Len()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := fmt.Sprintf("R-%d-%d", start, i)
				if _, err := e.Receive(Event{ID: "ev-" + id, Origin: "bench", ForwardedBy: "u", ForwardedAt: e.Now(), Ticket: benchClosedTicket(e, id)}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// seedFinishedInvestigations adds n finished investigations and their runs to
// the memory archive: the bulk of a long-running deployment's history.
func seedFinishedInvestigations(b *testing.B, n int) *Engine {
	b.Helper()
	cacheKey := fmt.Sprintf("worker/%d", n)
	if e, ok := seeded.Load(cacheKey); ok {
		return e.(*Engine)
	}
	s := seedMemoryStore(b, n).Store.Snapshot().detach()
	for i := 0; i < n; i++ {
		taskID, runID := fmt.Sprintf("inv-%d", i), fmt.Sprintf("run-%d", i)
		s.Investigations.Put(taskID, Investigation{ID: taskID, RunID: runID, ProjectID: "1", State: "agent_proposed", Phase: "assignment"})
		s.Runs.Put(runID, Run{ID: runID, TicketID: fmt.Sprintf("K-%d", i), InvestigationID: taskID, State: "agent_proposed"})
	}
	store, err := newStore(discardPersister{initial: s})
	if err != nil {
		b.Fatal(err)
	}
	e := testEngine(b)
	e.Store = store
	seeded.Store(cacheKey, e)
	return e
}

// BenchmarkMemoryWorkerClaim is one worker pass that finds one queued
// investigation among n finished ones: the idle check, then the claim
// transaction (recovery, slot counting, fair ordering). The candidate has no
// group, so the pass supersedes it instead of invoking an analyzer.
func BenchmarkMemoryWorkerClaim(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seedFinishedInvestigations(b, n)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := fmt.Sprintf("queued-%d-%d", n, i)
				if err := e.Store.Update(func(s *State) error {
					s.Investigations.Put(id, Investigation{ID: id, ProjectID: "1", State: "queued", CreatedAt: e.Now()})
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				if found, err := e.ProcessOne(ctx, nil); err != nil || found {
					b.Fatal(found, err)
				}
				if e.Store.Snapshot().Investigations.Get(id).State != "superseded" {
					b.Fatal("the worker did not reach the queued investigation")
				}
			}
		})
	}
}
