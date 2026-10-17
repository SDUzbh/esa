package esa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every store-backed test needs ESA_TEST_DATABASE_URL, since ESA keeps its
// state only in Postgres; scripts/test_postgres.sh starts a disposable server.
func testDatabaseURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("ESA_TEST_DATABASE_URL")
	if url == "" {
		t.Fatal(missingTestDatabase)
	}
	return url
}

const missingTestDatabase = "ESA_TEST_DATABASE_URL is required: ESA keeps its state in Postgres. Start a disposable server with scripts/test_postgres.sh"

var pgSchemaSeq atomic.Int64

// testSchema returns a fresh schema name, dropped when the test ends.
func testSchema(t testing.TB, url string) string {
	t.Helper()
	schema := fmt.Sprintf("t_%d_%d", time.Now().UnixNano()%1e9, pgSchemaSeq.Add(1))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if conn, err := pgx.Connect(ctx, url); err == nil {
			_, _ = conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			conn.Close(ctx)
		}
	})
	return schema
}

func openTestPostgres(t testing.TB, url, schema string) *PostgresPersister {
	t.Helper()
	p, err := OpenPostgres(context.Background(), url, PostgresOptions{Schema: schema, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func openTestPostgresStore(t testing.TB, p *PostgresPersister) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := OpenPostgresStore(ctx, p, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.verifyPublished)
	return s
}

// pgEngine is testEngine backed by a fresh Postgres schema.
func pgEngine(t *testing.T) (*Engine, *PostgresPersister, string, string) {
	t.Helper()
	url := testDatabaseURL(t)
	schema := testSchema(t, url)
	p := openTestPostgres(t, url, schema)
	e := testEngine(t)
	e.Store = openTestPostgresStore(t, p)
	return e, p, url, schema
}

func adminExec(t testing.TB, url, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func adminQueryInt(t testing.TB, url, sql string, args ...any) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int64
	if err = conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func requireReloadMatches(t testing.TB, p *PostgresPersister, want State) {
	t.Helper()
	got, _, _, err := p.load()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(hotProjection(want))
	b, _ := json.Marshal(hotProjection(got))
	if string(a) != string(b) {
		t.Fatalf("reload differs from the committed state\nwant %.400s\n got %.400s", a, b)
	}
}

// newTestStore backs testEngine: every test runs on its own Postgres schema,
// and after every commit a reload must reproduce the committed state (hot
// projection).
func newTestStore(t testing.TB) *Store {
	t.Helper()
	url := testDatabaseURL(t)
	p := openTestPostgres(t, url, testSchema(t, url))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.AcquireWriter(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	s, err := newStore(reloadChecking{p})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// reopenTestStore simulates a restart: it releases s's writer role and loads
// the same schema into a new Store. Do not use s afterwards.
func reopenTestStore(t testing.TB, s *Store) *Store {
	t.Helper()
	var p *PostgresPersister
	switch q := s.persister.(type) {
	case reloadChecking:
		p = q.PostgresPersister
	case *PostgresPersister:
		p = q
	default:
		t.Fatalf("store has no Postgres persister: %T", s.persister)
	}
	p.Close()
	return openTestPostgresStore(t, openTestPostgres(t, p.url, p.schema))
}

// reloadChecking verifies every commit by reloading it from the database.
type reloadChecking struct{ *PostgresPersister }

func (r reloadChecking) Apply(c Commit) (uint64, error) {
	revision, err := r.PostgresPersister.Apply(c)
	if err != nil {
		return revision, err
	}
	got, loaded, _, err := r.PostgresPersister.load()
	if err != nil {
		panic(fmt.Sprintf("esa: reload after commit failed: %v", err))
	}
	a, _ := json.Marshal(hotProjection(c.State))
	b, _ := json.Marshal(hotProjection(got))
	if loaded != revision || string(a) != string(b) {
		panic(fmt.Sprintf("esa: Postgres reload differs from the committed state at revision %d\nwant %.600s\n got %.600s", revision, a, b))
	}
	return revision, nil
}

func TestPostgresTablesCoverEveryMap(t *testing.T) {
	names := map[string]bool{}
	for _, table := range pgTables {
		names[table.name] = true
	}
	typ := reflect.TypeOf(State{})
	tables := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.IsExported() && reflect.PointerTo(f.Type).Implements(reflect.TypeFor[stateTable]()) {
			tables++
			if !names[name] {
				t.Errorf("State.%s (%s) has no Postgres table", f.Name, name)
			}
		}
	}
	if tables != len(pgTables) {
		t.Errorf("State has %d tables, pgTables lists %d", tables, len(pgTables))
	}
	sql, _ := pgMigrations.ReadFile("pg_migrations/001_initial.sql")
	for name := range names {
		if !strings.Contains(string(sql), "{{schema}}."+name+" (") {
			t.Errorf("migration 001 creates no table %s", name)
		}
	}
}

func TestPostgresRoundTripAndOnlyChangedRows(t *testing.T) {
	e, p, url, schema := pgEngine(t)
	ticket, run := analyzedTicket(t, e, "analyzed")
	receive(t, e, "open", fixture(e, "open"))
	closed := fixture(e, "closed")
	closed.Closed, closed.Status = true, "Resolved"
	closed.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	receive(t, e, "closed", closed)
	requireReloadMatches(t, p, e.Store.Snapshot())
	if run.IssueID == "" || ticket.ID == "" {
		t.Fatal("fixture has no analyzed Issue")
	}
	before := e.Store.revision
	if err := e.Store.Update(func(*State) error { return nil }); err != nil || e.Store.revision != before {
		t.Fatal("no-op Update wrote a revision", err, e.Store.revision, before)
	}
	if err := e.update(func(s *State) error {
		tk := s.Tickets.Get("open")
		tk.Assignee = "engineer"
		s.Tickets.Put("open", tk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rev := int64(e.Store.revision)
	if rev != int64(before)+1 {
		t.Fatal("changed Update did not advance one revision", rev, before)
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+pgx.Identifier{schema, "tickets"}.Sanitize()+" WHERE revision = $1", rev); n != 1 {
		t.Fatal("a one-ticket Update rewrote other ticket rows", n)
	}
	requireReloadMatches(t, p, e.Store.Snapshot())
}

func TestPostgresNormalizesRunsEventsAndColdSnapshots(t *testing.T) {
	e, p, url, schema := pgEngine(t)
	open := forward(e, "open-event", fixture(e, "open"))
	open.Raw = json.RawMessage(`{"from":"jira"}`)
	if _, err := e.Receive(open); err != nil {
		t.Fatal(err)
	}
	closed := fixture(e, "closed")
	closed.Closed, closed.Status = true, "Resolved"
	closed.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	receive(t, e, "closed-event", closed)
	s := e.Store.Snapshot()
	runID := s.Latest.Get("open")
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+table("runs")+" WHERE id = $1 AND payload->'input' IS NULL", runID); n != 1 {
		t.Fatal("run input was stored instead of referencing its snapshot")
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+table("events")+" WHERE payload->'raw' IS NULL AND id = 'open-event'"); n != 1 {
		t.Fatal("raw payload stored inline with the event")
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+table("event_raw")+" WHERE event_id = 'open-event' AND raw::text = $1", `{"from":"jira"}`); n != 1 {
		t.Fatal("raw payload not kept verbatim in event_raw")
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+table("membership_history")); n == 0 {
		t.Fatal("membership history not persisted")
	}
	loaded, _, _, err := p.load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Runs.Get(runID).Input.ID != "open" || hash(loaded.Runs.Get(runID).Input) != hash(s.Runs.Get(runID).Input) {
		t.Fatal("run input was not restored from its snapshot")
	}
	if _, ok := loaded.Snapshots.Lookup(s.LatestSnapshot.Get("closed")); ok {
		t.Fatal("a closed ticket's snapshot, referenced by no run or link, was loaded")
	}
	if _, ok := loaded.Snapshots.Lookup(s.Runs.Get(runID).SnapshotID); !ok {
		t.Fatal("a run's snapshot was not loaded")
	}
	if loaded.Events.Get("open-event").Raw != nil || len(loaded.MembershipHistory) != 0 {
		t.Fatal("cold data was loaded")
	}
	// Idempotency on the reloaded state.
	reloaded := testEngine(t)
	p2 := openTestPostgres(t, url, schema)
	p.Close()
	reloaded.Store = openTestPostgresStore(t, p2)
	if receipt, err := reloaded.Receive(open); err != nil || receipt.Action != "duplicate" {
		t.Fatal("reloaded state no longer deduplicates", receipt.Action, err)
	}
	// Appends after a reload continue the unloaded membership history.
	before := adminQueryInt(t, url, "SELECT count(*) FROM "+table("membership_history"))
	reforward := fixture(reloaded, "open")
	reforward.Description += " changed scope"
	receive(t, reloaded, "open-event-2", reforward)
	if after := adminQueryInt(t, url, "SELECT max(seq) + 1 FROM "+table("membership_history")); after <= before {
		t.Fatal("membership history append reused a sequence", before, after)
	}
}

func TestPostgresImmutableRowsAndAtomicFailure(t *testing.T) {
	e, p, url, schema := pgEngine(t)
	receive(t, e, "e1", fixture(e, "T1"))
	snapID := e.Store.Snapshot().LatestSnapshot.Get("T1")
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, "UPDATE "+pgx.Identifier{schema, "snapshots"}.Sanitize()+" SET payload = '{}' WHERE id = $1", snapID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatal("snapshot row could be modified", err)
	}
	err = e.Store.Update(func(s *State) error {
		snap := s.Snapshots.Get(snapID)
		snap.Ticket.Subject = "rewritten"
		s.Snapshots.Put(snapID, snap)
		return nil
	})
	if err == nil || e.Store.Snapshot().Snapshots.Get(snapID).Ticket.Subject == "rewritten" {
		t.Fatal("snapshot rewrite was committed", err)
	}
	// A failure inside the transaction leaves database and memory unchanged.
	rev := e.Store.revision
	p.testBeforeCommit = func() error { return errors.New("injected failure") }
	err = e.Store.Update(func(s *State) error { s.Tickets.Put("T2", fixture(e, "T2")); return nil })
	p.testBeforeCommit = nil
	if err == nil || e.Store.revision != rev || e.Store.Snapshot().Tickets.Len() != 1 {
		t.Fatal("failed transaction changed memory", err)
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+pgx.Identifier{schema, "tickets"}.Sanitize()); n != 1 {
		t.Fatal("failed transaction left rows behind", n)
	}
	if n := adminQueryInt(t, url, "SELECT revision FROM "+pgx.Identifier{schema, "meta"}.Sanitize()); n != int64(rev) {
		t.Fatal("failed transaction advanced the revision", n, rev)
	}
}

func TestPostgresUnknownCommitOutcome(t *testing.T) {
	e, p, _, _ := pgEngine(t)
	receive(t, e, "e1", fixture(e, "T1"))
	// The commit succeeded but its reply was lost: meta.revision shows it landed.
	rev := e.Store.revision
	p.testAfterCommit = func() error { return errors.New("connection reset by peer") }
	err := e.Store.Update(func(s *State) error { s.Tickets.Put("T2", fixture(e, "T2")); return nil })
	p.testAfterCommit = nil
	if err != nil || e.Store.revision != rev+1 || e.Store.Snapshot().Tickets.Get("T2").ID != "T2" {
		t.Fatal("committed Update with a lost reply was not published", err)
	}
	// Nothing committed: the revision is unchanged, so the error stands.
	lost := errors.New("connection reset by peer")
	if _, err = p.resolveCommit(e.Store.revision, lost); !errors.Is(err, lost) {
		t.Fatal("uncommitted outcome was not reported as a failure", err)
	}
	// A revision that moved on otherwise means another writer.
	if _, err = p.resolveCommit(e.Store.revision-2, lost); !errors.Is(err, ErrLostWriter) {
		t.Fatal("foreign revision not treated as a lost writer", err)
	}
}

func lockPID(t testing.TB, p *PostgresPersister) uint32 {
	t.Helper()
	if p.lockPID == 0 {
		t.Fatal("no lock session")
	}
	return p.lockPID
}

func TestPostgresStandbyWaitsUntilWriterReleases(t *testing.T) {
	url := testDatabaseURL(t)
	schema := testSchema(t, url)
	a := openTestPostgres(t, url, schema)
	openTestPostgresStore(t, a)
	b := openTestPostgres(t, url, schema)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := b.AcquireWriter(ctx, 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("standby acquired the writer lock while it was held", err)
	}
	a.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := b.AcquireWriter(ctx2, 20*time.Millisecond); err != nil || b.term != a.term+1 {
		t.Fatal("standby did not take over after release", err, a.term, b.term)
	}
}

// Bump first: once a new writer bumps the term, the old writer cannot commit.
func TestPostgresTakeoverFencesOldWriter(t *testing.T) {
	e, a, url, schema := pgEngine(t)
	receive(t, e, "e1", fixture(e, "T1"))
	adminExec(t, url, "SELECT pg_terminate_backend($1)", lockPID(t, a))
	b := openTestPostgres(t, url, schema)
	newWriter := openTestPostgresStore(t, b)
	err := e.Store.Update(func(s *State) error { s.Tickets.Put("T2", fixture(e, "T2")); return nil })
	if !errors.Is(err, ErrLostWriter) {
		t.Fatal("old writer committed after the term bump", err)
	}
	if _, ok := e.Store.Snapshot().Tickets.Lookup("T2"); ok {
		t.Fatal("fenced Update changed the old writer's memory")
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+pgx.Identifier{schema, "tickets"}.Sanitize()); n != 1 {
		t.Fatal("fenced Update reached the database", n)
	}
	if newWriter.Snapshot().Tickets.Get("T1").ID != "T1" {
		t.Fatal("new writer did not load earlier commits")
	}
	if err = a.CheckLock(context.Background()); !errors.Is(err, ErrLostWriter) {
		t.Fatal("lock check did not report the lost lock", err)
	}
}

// Old commit first: a commit holding the meta row before the bump lands, and
// the new writer, which loads after its bump, sees it.
func TestPostgresCommitBeforeBumpIsSeenByNewWriter(t *testing.T) {
	e, a, url, schema := pgEngine(t)
	receive(t, e, "e1", fixture(e, "T1"))
	paused, release := make(chan struct{}), make(chan struct{})
	a.testBeforeCommit = func() error { close(paused); <-release; return nil }
	committed := make(chan error, 1)
	go func() {
		committed <- e.Store.Update(func(s *State) error { s.Tickets.Put("T2", fixture(e, "T2")); return nil })
	}()
	<-paused
	adminExec(t, url, "SELECT pg_terminate_backend($1)", lockPID(t, a))
	b := openTestPostgres(t, url, schema)
	opened := make(chan *Store, 1)
	go func() {
		s, err := OpenPostgresStore(context.Background(), b, 20*time.Millisecond)
		if err != nil {
			t.Error(err)
		}
		opened <- s
	}()
	// Wait until the new writer's term bump is blocked on the old writer's row lock.
	deadline := time.Now().Add(5 * time.Second)
	for adminQueryInt(t, url, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%writer_term = writer_term + 1%'") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("new writer never waited on the meta row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatal("commit that held the meta row before the bump failed", err)
	}
	s := <-opened
	if s == nil || s.Snapshot().Tickets.Get("T2").ID != "T2" {
		t.Fatal("new writer missed a commit that landed before its term bump")
	}
}

func TestPostgresWatchLockReportsLoss(t *testing.T) {
	_, a, url, _ := pgEngine(t)
	lost := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.WatchLock(ctx, 20*time.Millisecond, func(err error) { lost <- err })
	adminExec(t, url, "SELECT pg_terminate_backend($1)", lockPID(t, a))
	select {
	case err := <-lost:
		if !errors.Is(err, ErrLostWriter) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock loss not reported")
	}
}

// hotProjection is the part of s that a working-set Load reproduces: snapshots
// referenced in memory, no membership history and no raw event payloads.
func hotProjection(s State) State {
	out := s.detach()
	hot := map[string]bool{}
	for _, id := range hotSnapshotIDs(&s) {
		hot[id] = true
	}
	for id := range out.Snapshots.All() {
		if !hot[id] {
			out.Snapshots.Delete(id)
		}
	}
	for id, r := range out.Events.All() {
		r.Raw = nil
		out.Events.Put(id, r)
	}
	out.MembershipHistory = nil
	return out
}
