package esa

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// TestArchiveHeapProbe ingests a preserved Jira archive into Postgres,
// reloads it the way a restart does, and reports the live heap and how much
// of it each State table holds. It is a measurement, not a check, and runs
// only with ESA_ARCHIVE_PROBE set to the archive's raw directory. Open
// tickets get a real Agent report from ESA_ARCHIVE_REPORT, if set, so the
// heap reflects an analyzed archive rather than intake alone.
func TestArchiveHeapProbe(t *testing.T) {
	raw := os.Getenv("ESA_ARCHIVE_PROBE")
	if raw == "" {
		t.Skip("set ESA_ARCHIVE_PROBE to a Jira raw/ directory")
	}
	defer func(previous bool) { guardPublishedState = previous }(guardPublishedState)
	guardPublishedState = false
	var paths []string
	if err := filepath.WalkDir(raw, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".json" {
			paths = append(paths, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var tickets []Ticket
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err = json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		ticket, err := parseJira(record)
		if err != nil {
			continue
		}
		ticket.ProjectID = "1"
		tickets = append(tickets, ticket)
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].CreatedAt.Before(tickets[j].CreatedAt) })
	var report Report
	if path := os.Getenv("ESA_ARCHIVE_REPORT"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
	}
	url := testDatabaseURL(t)
	p := openTestPostgres(t, url, testSchema(t, url))
	// No published-state guard: it is off for the ingest, so its check at
	// cleanup would not hold.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	store, err := OpenPostgresStore(ctx, p, 20*time.Millisecond)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile("../../config/rules.json")
	var config Config
	if err := json.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(store, config)
	if err != nil {
		t.Fatal(err)
	}
	e.IssueDirectory = t.TempDir()
	started := time.Now()
	open := 0
	for i, ticket := range tickets {
		at := ticket.UpdatedAt.Add(time.Second)
		e.Now = func() time.Time { return at }
		r, err := e.Receive(Event{ID: fmt.Sprintf("probe-%d", i), Origin: "probe", ForwardedBy: "probe", ForwardedAt: at, Ticket: ticket})
		if err != nil {
			t.Fatal(ticket.SourceKey, err)
		}
		if ticket.Closed || r.RunID == "" {
			continue
		}
		open++
		if report.Summary != "" && open%2 == 0 {
			if err = e.update(func(s *State) error {
				run := s.Runs.Get(r.RunID)
				run.Report = report
				s.Runs.Put(run.ID, run)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("ingested %d tickets (%d open) in %s", len(tickets), open, time.Since(started).Round(time.Second))
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	// The ingest store may stay reachable from its background goroutines; the
	// load below decodes a fresh copy, so its heap delta is what a restart holds.
	e, store = nil, nil
	before := heap()
	loadStarted := time.Now()
	state, _, err := p.Load()
	if err != nil {
		t.Fatal(err)
	}
	loaded := heap()
	t.Logf("load %s, live heap %.0f MB", time.Since(loadStarted).Round(time.Millisecond), float64(loaded-before)/1e6)
	// Drop one table at a time and measure what it freed. Strings shared
	// between tables (a run's input and its snapshot) count where the last
	// reference goes.
	type step struct {
		name string
		drop func(*State)
	}
	steps := []step{
		{"runs (reports and inputs)", func(s *State) { s.Runs = Table[Run]{} }},
		{"snapshots (hot)", func(s *State) { s.Snapshots = Table[TicketSnapshot]{} }},
		{"tickets", func(s *State) { s.Tickets = Table[Ticket]{} }},
		{"events", func(s *State) { s.Events = Table[EventRecord]{} }},
		{"receipts", func(s *State) { s.Receipts = Table[Receipt]{} }},
		{"investigations", func(s *State) { s.Investigations = Table[Investigation]{} }},
		{"groups", func(s *State) { s.Groups = Table[InvestigationGroup]{} }},
		{"cases and case history", func(s *State) { s.Cases = Table[Case]{}; s.CaseHistory = nil }},
		{"latest maps", func(s *State) { s.Latest = Table[string]{}; s.LatestSnapshot = Table[string]{} }},
	}
	last := loaded
	for _, st := range steps {
		st.drop(&state)
		now := heap()
		t.Logf("  %-28s %7.1f MB", st.name, (float64(last)-float64(now))/1e6)
		last = now
	}
	t.Logf("  %-28s %7.1f MB", "rest", (float64(last)-float64(before))/1e6)
	runtime.KeepAlive(state)
}
