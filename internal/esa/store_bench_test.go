package esa

// Store scaling benchmarks (REVIEW_ISSUES.md Issue 1). They are not run by
// go test without -bench. Example:
//
//	go test ./internal/esa -run '^$' -bench 'Store|TicketGET|ReceiveClosed' -benchtime 5x
//
// ESA_BENCH_SIZES overrides the ticket counts, e.g. ESA_BENCH_SIZES=1000,10000.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
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
		event, snapshot, c := s.Events["ev-0"], s.Snapshots[s.LatestSnapshot["K-0"]], s.Cases[receipt.CaseID]
		for i := 1; i < n; i++ {
			ticketID, eventID := fmt.Sprintf("K-%d", i), fmt.Sprintf("ev-%d", i)
			ticket := s.Tickets["K-0"]
			ticket.ID, ticket.SourceKey = ticketID, ticketID
			s.Tickets[ticketID] = ticket
			event.ID, event.TicketID, event.TicketHash, event.SnapshotID = eventID, ticketID, hash(ticket), snapshotIDFor(eventID)
			s.Events[eventID] = event
			snapshot.ID, snapshot.EventID, snapshot.Ticket, snapshot.Hash = "snapshot_"+hash(eventID), eventID, ticket, hash(ticket)
			s.Snapshots[snapshot.ID] = snapshot
			s.LatestSnapshot[ticketID] = snapshot.ID
			c.ID, c.TicketID, c.SourceKey = "case_"+hash(ticketID)[:24], ticketID, ticketID
			s.Cases[c.ID] = c
			s.CaseHistory = append(s.CaseHistory, c)
			s.Receipts[eventID] = Receipt{EventID: eventID, TicketID: ticketID, CaseID: c.ID, Action: receipt.Action}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if got := len(e.Store.Snapshot().Tickets); got != n {
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
					ticket := s.Tickets["K-1"]
					ticket.Assignee = fmt.Sprint(i)
					s.Tickets[ticket.ID] = ticket
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
			start := len(e.Store.Snapshot().Tickets)
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
