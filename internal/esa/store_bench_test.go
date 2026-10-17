package esa

// Store scaling benchmarks (REVIEW_ISSUES.md Issue 1). They are not run by
// go test without -bench. Example:
//
//	go test ./internal/esa -run '^$' -bench 'Store|TicketGET|ReceiveClosed' -benchtime 5x
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

// seedClosedTickets receives one real closed ticket, then copies its stored
// entities under new IDs. Receiving every ticket would take quadratic time.
func seedClosedTickets(b *testing.B, n int) *Engine {
	b.Helper()
	e := testEngine(b)
	template := benchClosedTicket(e, "K-0")
	receipt, err := e.Receive(Event{ID: "ev-0", Origin: "bench", ForwardedBy: "u", ForwardedAt: e.Now(), Ticket: template})
	if err != nil {
		b.Fatal(err)
	}
	err = e.Store.Update(func(s *State) error {
		event, snapshot, c := s.Events.Get("ev-0"), s.Snapshots.Get(s.LatestSnapshot.Get("K-0")), s.Cases.Get(receipt.CaseID)
		for i := 1; i < n; i++ {
			ticketID, eventID := fmt.Sprintf("K-%d", i), fmt.Sprintf("ev-%d", i)
			ticket := s.Tickets.Get("K-0")
			ticket.ID, ticket.SourceKey = ticketID, ticketID
			s.Tickets.Put(ticketID, ticket)
			event.ID, event.TicketID, event.TicketHash, event.SnapshotID = eventID, ticketID, hash(ticket), snapshotIDFor(eventID)
			s.Events.Put(eventID, event)
			snapshot.ID, snapshot.EventID, snapshot.Ticket, snapshot.Hash = "snapshot_"+hash(eventID), eventID, ticket, hash(ticket)
			s.Snapshots.Put(snapshot.ID, snapshot)
			s.LatestSnapshot.Put(ticketID, snapshot.ID)
			c.ID, c.TicketID, c.SourceKey = "case_"+hash(ticketID)[:24], ticketID, ticketID
			s.Cases.Put(c.ID, c)
			s.CaseHistory = append(s.CaseHistory, c)
			s.Receipts.Put(eventID, Receipt{EventID: eventID, TicketID: ticketID, CaseID: c.ID, Action: receipt.Action})
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if got := e.Store.Snapshot().Tickets.Len(); got != n {
		b.Fatalf("seeded %d tickets, want %d", got, n)
	}
	return e
}

// seeded builds each size once per benchmark binary run; Go invokes a
// benchmark function several times while it calibrates b.N.
var seeded sync.Map

func seededEngine(b *testing.B, key string, n int) *Engine {
	b.Helper()
	cacheKey := fmt.Sprintf("%s/%d", key, n)
	if e, ok := seeded.Load(cacheKey); ok {
		return e.(*Engine)
	}
	e := seedClosedTickets(b, n)
	seeded.Store(cacheKey, e)
	return e
}

func BenchmarkStoreSnapshot(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seededEngine(b, "read", n)
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
			e := seededEngine(b, "read", n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.Store.Update(func(*State) error { return nil }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkStoreUpdateOneTicket(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seededEngine(b, "write", n)
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

func BenchmarkReceiveClosedTicket(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := seededEngine(b, "receive", n)
			start := e.Store.Snapshot().Tickets.Len()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := fmt.Sprintf("R-%d", start+i)
				if _, err := e.Receive(Event{ID: "ev-" + id, Origin: "bench", ForwardedBy: "u", ForwardedAt: e.Now(), Ticket: benchClosedTicket(e, id)}); err != nil {
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
			e := seededEngine(b, "read", n)
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

// BenchmarkTicketsList measures the plugin's ESA index page: GET /v1/tickets
// with no query parameters (default sort updated_at desc, page 1 of 25). The
// plugin always scopes the request to the viewer's visible issues and refuses
// more than 2,000 of them, so "scoped" sends 2,000 ticket IDs spread over the
// archive; "unscoped" omits X-ESA-Ticket-IDs, which lets every ticket through.
func BenchmarkTicketsList(b *testing.B) {
	for _, n := range benchSizes(b) {
		visible := pluginTicketScope(n)
		for _, scope := range []struct {
			name string
			ids  []string
		}{{"scoped", visible}, {"unscoped", nil}} {
			b.Run(fmt.Sprintf("%s/%d", scope.name, n), func(b *testing.B) {
				e := seedMemoryStore(b, n)
				handler := (&API{Engine: e, Token: strings.Repeat("t", 24)}).Handler()
				want := n
				if scope.ids != nil {
					want = len(scope.ids)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					req := httptest.NewRequest(http.MethodGet, "/v1/tickets", nil)
					req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 24))
					req.Header.Set("X-ESA-Project-IDs", "1")
					if scope.ids != nil {
						req.Header.Set("X-ESA-Ticket-IDs", strings.Join(scope.ids, ","))
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, req)
					if w.Code != http.StatusOK {
						b.Fatalf("GET returned %d: %s", w.Code, w.Body.String())
					}
					if i == 0 && !strings.Contains(w.Body.String(), fmt.Sprintf(`"total_count":%d`, want)) {
						b.Fatalf("GET did not list %d tickets: %.300s", want, w.Body.String())
					}
				}
			})
		}
	}
}

// pluginTicketScope is the plugin's largest X-ESA-Ticket-IDs scope: 2,000
// ticket IDs spread evenly over an archive of n, starting with K-0.
func pluginTicketScope(n int) []string {
	ids := make([]string, 0, 2000)
	for i := 0; i < 2000 && i < n; i++ {
		ids = append(ids, fmt.Sprintf("K-%d", i*n/min(n, 2000)))
	}
	return ids
}

// BenchmarkSolutionHistory measures GET /v1/investigations/{id}/solutions,
// the plugin's solution history, in an archive where each of n tickets has an
// investigation with one solution revision.
func BenchmarkSolutionHistory(b *testing.B) {
	for _, n := range benchSizes(b) {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			cacheKey := fmt.Sprintf("solutions/%d", n)
			if _, ok := seeded.Load(cacheKey); !ok {
				s := seedFinishedInvestigations(b, n).Store.Snapshot().detach()
				for i := 0; i < n; i++ {
					taskID, solutionID := fmt.Sprintf("inv-%d", i), fmt.Sprintf("sol-%d", i)
					task := s.Investigations.Get(taskID)
					task.SolutionID = solutionID
					s.Investigations.Put(taskID, task)
					s.Solutions.Put(solutionID, SolutionRevision{ID: solutionID, InvestigationID: taskID, ProjectID: "1", SourceTicketID: fmt.Sprintf("K-%d", i), State: "agent_proposed", Revision: 1})
				}
				store, err := newStore(discardPersister{initial: s})
				if err != nil {
					b.Fatal(err)
				}
				e := testEngine(b)
				e.Store = store
				seeded.Store(cacheKey, e)
			}
			e, _ := seeded.Load(cacheKey)
			handler := (&API{Engine: e.(*Engine), Token: strings.Repeat("t", 24)}).Handler()
			scope := strings.Join(pluginTicketScope(n), ",")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(http.MethodGet, "/v1/investigations/inv-0/solutions", nil)
				req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 24))
				req.Header.Set("X-ESA-Project-IDs", "1")
				req.Header.Set("X-ESA-Ticket-IDs", scope)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"sol-0"`) {
					b.Fatalf("GET returned %d: %.300s", w.Code, w.Body.String())
				}
			}
		})
	}
}

// BenchmarkPostgresUpdateOneTicket measures a one-ticket write on the Postgres
// store; it needs ESA_TEST_DATABASE_URL and skips without it.
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
