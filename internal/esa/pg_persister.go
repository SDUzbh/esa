package esa

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrLostWriter means another process became the writer. The state in this
// process can no longer be committed; the service must stop and restart.
var ErrLostWriter = errors.New("esa: this process is no longer the state writer")

//go:embed pg_migrations/*.sql
var pgMigrations embed.FS

var pgSchemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// PostgresOptions configures a PostgresPersister. Zero values select defaults.
type PostgresOptions struct {
	// Schema holds ESA's tables; "esa" by default.
	Schema string
	// Timeout bounds each Load and Apply; 30 s by default.
	Timeout time.Duration
}

// PostgresPersister stores ESA state as one row per entity in an ESA-owned
// schema (docs/STORAGE_DESIGN.md). Only one process writes at a time:
// AcquireWriter takes a session advisory lock and bumps meta.writer_term, and
// every Apply is fenced on that term, so a process that lost the lock can
// never commit. Snapshots referenced by no run, contract or live membership,
// membership history and raw event payloads are written but not loaded.
type PostgresPersister struct {
	url, schema string
	timeout     time.Duration
	conn        *pgx.Conn // data; used under Store.mu, one Apply at a time
	lock        *pgx.Conn // holds the advisory lock; used by AcquireWriter and CheckLock
	lockPID     uint32
	term        uint64
	// membershipBase counts membership history rows that exist in the
	// database but are not loaded; appended rows continue after them.
	membershipBase int64

	// Test hooks, nil in production.
	testBeforeCommit       func() error
	testAfterCommit        func() error
	testBeforeImportVerify func(context.Context, pgx.Tx) error
}

// OpenPostgres connects the data and lock sessions. Call AcquireWriter before
// Load or Apply.
func OpenPostgres(ctx context.Context, url string, opts PostgresOptions) (*PostgresPersister, error) {
	p := &PostgresPersister{url: url, schema: opts.Schema, timeout: opts.Timeout}
	if p.schema == "" {
		p.schema = "esa"
	}
	if p.timeout == 0 {
		p.timeout = 30 * time.Second
	}
	if !pgSchemaName.MatchString(p.schema) {
		return nil, fmt.Errorf("%w: invalid Postgres schema name %q", ErrInvalid, p.schema)
	}
	var err error
	if p.conn, err = pgx.Connect(ctx, url); err != nil {
		return nil, err
	}
	if p.lock, err = pgx.Connect(ctx, url); err != nil {
		p.conn.Close(ctx)
		return nil, err
	}
	// Keep the lock session alive while idle, and let the server notice a dead
	// peer within about 25 s so a standby can take over.
	if _, err = p.lock.Exec(ctx, "SET idle_session_timeout = 0; SET tcp_keepalives_idle = 10; SET tcp_keepalives_interval = 5; SET tcp_keepalives_count = 3"); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// Close ends both sessions, which releases the advisory lock.
func (p *PostgresPersister) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p.conn != nil {
		p.conn.Close(ctx)
	}
	if p.lock != nil {
		p.lock.Close(ctx)
	}
}

func (p *PostgresPersister) table(name string) string {
	return pgx.Identifier{p.schema, name}.Sanitize()
}

// lockKey is the advisory lock for this schema's writer.
func (p *PostgresPersister) lockKey() int64 {
	h := fnv.New64a()
	h.Write([]byte("esa-writer:" + p.schema))
	return int64(h.Sum64())
}

// AcquireWriter waits until this process holds the writer lock, polling every
// poll interval (a standby waits here), then applies schema migrations and
// bumps writer_term in its own committed transaction. Only after that may the
// state be loaded: the new term fences every older writer, and loading after
// the bump sees everything they committed.
func (p *PostgresPersister) AcquireWriter(ctx context.Context, poll time.Duration) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// pgx closes a connection whose context ends mid-query, so the caller's
		// context is checked only between polls; a cancelled standby keeps a
		// usable lock session.
		query, cancel := context.WithTimeout(context.Background(), p.timeout)
		var held bool
		err := p.lock.QueryRow(query, "SELECT pg_try_advisory_lock($1)", p.lockKey()).Scan(&held)
		cancel()
		if err != nil {
			return err
		}
		if held {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	if err := p.lock.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p.lockPID); err != nil {
		return err
	}
	if err := p.migrate(ctx); err != nil {
		return err
	}
	var term int64
	if err := p.conn.QueryRow(ctx, "UPDATE "+p.table("meta")+" SET writer_term = writer_term + 1 RETURNING writer_term").Scan(&term); err != nil {
		return err
	}
	p.term = uint64(term)
	return nil
}

// CheckLock verifies that the lock session is still the one that took the
// lock and still holds it. Any error means the lock may be lost.
func (p *PostgresPersister) CheckLock(ctx context.Context) error {
	var pid uint32
	var held bool
	err := p.lock.QueryRow(ctx, `SELECT pg_backend_pid(), EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted AND objsubid = 1
		AND ((classid::bigint << 32) | objid::bigint) = $1)`, p.lockKey()).Scan(&pid, &held)
	if err != nil {
		return fmt.Errorf("%w: lock session failed: %v", ErrLostWriter, err)
	}
	if pid != p.lockPID || !held {
		return fmt.Errorf("%w: advisory lock no longer held", ErrLostWriter)
	}
	return nil
}

// WatchLock checks the lock every interval until ctx ends, and calls lost
// once if a check fails. The caller must then stop serving and exit.
func (p *PostgresPersister) WatchLock(ctx context.Context, interval time.Duration, lost func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		check, cancel := context.WithTimeout(ctx, interval)
		err := p.CheckLock(check)
		cancel()
		if err != nil && ctx.Err() == nil {
			lost(err)
			return
		}
	}
}

func (p *PostgresPersister) migrate(ctx context.Context) error {
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{p.schema}.Sanitize()+"; CREATE TABLE IF NOT EXISTS "+p.table("schema_migrations")+" (version int PRIMARY KEY, applied_at timestamptz NOT NULL)"); err != nil {
		return err
	}
	applied := map[int]bool{}
	rows, err := tx.Query(ctx, "SELECT version FROM "+p.table("schema_migrations"))
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err = rows.Scan(&v); err != nil {
			return err
		}
		applied[v] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	names, err := fs.Glob(pgMigrations, "pg_migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "pg_migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if applied[version] {
			continue
		}
		sql, err := pgMigrations.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, strings.ReplaceAll(string(sql), "{{schema}}", pgx.Identifier{p.schema}.Sanitize())); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO "+p.table("schema_migrations")+" VALUES ($1, now())", version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// pgFrame is the meta.frame document: the scalar State fields plus unknown
// top-level fields kept from older states.
type pgFrame struct {
	IssueAccessSequence uint64     `json:"issue_access_sequence,omitempty"`
	ReplayClock         time.Time  `json:"replay_clock,omitzero"`
	LastClaimedProject  string     `json:"last_claimed_project,omitempty"`
	SchemaVersion       int        `json:"schema_version"`
	Unknown             JSONFields `json:"unknown,omitempty"`
}

// pgTable maps one State map to its table.
type pgTable struct {
	name   string
	ids    func(*State) []string
	encode func(*State, string) ([]byte, error)
	decode func(*State, string, []byte) error
}

func mapTable[V any](name string, field func(*State) *map[string]V) pgTable {
	return pgTable{
		name: name,
		ids: func(s *State) []string {
			ids := make([]string, 0, len(*field(s)))
			for id := range *field(s) {
				ids = append(ids, id)
			}
			return ids
		},
		encode: func(s *State, id string) ([]byte, error) { return json.Marshal((*field(s))[id]) },
		decode: func(s *State, id string, payload []byte) error {
			var v V
			if err := json.Unmarshal(payload, &v); err != nil {
				return fmt.Errorf("%s %s: %w", name, id, err)
			}
			if *field(s) == nil {
				*field(s) = map[string]V{}
			}
			(*field(s))[id] = v
			return nil
		},
	}
}

// pgTables lists every State map; TestPostgresTablesCoverEveryMap enforces it.
var pgTables = []pgTable{
	mapTable("latest_snapshot", func(s *State) *map[string]string { return &s.LatestSnapshot }),
	mapTable("snapshots", func(s *State) *map[string]TicketSnapshot { return &s.Snapshots }),
	mapTable("engineering_issues", func(s *State) *map[string]EngineeringIssue { return &s.Issues }),
	mapTable("issue_ticket_links", func(s *State) *map[string]IssueTicketLink { return &s.Memberships }),
	mapTable("issue_overrides", func(s *State) *map[string]string { return &s.IssueOverrides }),
	mapTable("investigations", func(s *State) *map[string]Investigation { return &s.Investigations }),
	mapTable("investigation_groups", func(s *State) *map[string]InvestigationGroup { return &s.Groups }),
	mapTable("solution_revisions", func(s *State) *map[string]SolutionRevision { return &s.Solutions }),
	mapTable("classifier_predictions", func(s *State) *map[string]ClassifierPrediction { return &s.ClassifierPredictions }),
	mapTable("classifier_feedback", func(s *State) *map[string]ClassifierFeedback { return &s.ClassifierFeedback }),
	mapTable("tickets", func(s *State) *map[string]Ticket { return &s.Tickets }),
	eventsTable(),
	mapTable("receipts", func(s *State) *map[string]Receipt { return &s.Receipts }),
	mapTable("event_rejections", func(s *State) *map[string]EventRejection { return &s.Rejections }),
	runsTable(),
	mapTable("latest", func(s *State) *map[string]string { return &s.Latest }),
	mapTable("cases", func(s *State) *map[string]Case { return &s.Cases }),
}

// eventsTable stores records without Raw; Raw goes to the cold event_raw table.
func eventsTable() pgTable {
	t := mapTable("events", func(s *State) *map[string]EventRecord { return &s.Events })
	t.encode = func(s *State, id string) ([]byte, error) {
		r := s.Events[id]
		r.Raw = nil
		return json.Marshal(r)
	}
	return t
}

// runsTable omits a run's input when its snapshot holds the identical ticket;
// Load restores it from that snapshot, which is always loaded.
func runsTable() pgTable {
	t := mapTable("runs", func(s *State) *map[string]Run { return &s.Runs })
	t.encode = func(s *State, id string) ([]byte, error) {
		run := s.Runs[id]
		b, err := json.Marshal(run)
		if err != nil || run.SnapshotID == "" {
			return b, err
		}
		if snap, ok := s.Snapshots[run.SnapshotID]; !ok || hash(run.Input) != snap.Hash {
			return b, nil
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(b, &fields); err != nil {
			return nil, err
		}
		delete(fields, "input")
		return json.Marshal(fields)
	}
	return t
}

func (p *PostgresPersister) withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), p.timeout)
}

// Load reads the in-memory working set in one consistent read-only snapshot
// of the database, and adopts its membership history position for later
// appends. It requires AcquireWriter.
func (p *PostgresPersister) Load() (State, uint64, error) {
	if p.term == 0 {
		return State{}, 0, errors.New("esa: Postgres Load before AcquireWriter")
	}
	state, revision, membershipRows, err := p.load()
	if err == nil {
		p.membershipBase = membershipRows
	}
	return state, revision, err
}

// load reads the working set without changing the persister.
func (p *PostgresPersister) load() (State, uint64, int64, error) {
	ctx, cancel := p.withTimeout()
	defer cancel()
	tx, err := p.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return State{}, 0, 0, err
	}
	defer tx.Rollback(ctx)
	return p.readState(ctx, tx, false)
}

// readState reads the state through tx. The working set omits snapshots that
// nothing in memory references, membership history and raw event payloads;
// full also reads them, to reconstruct the complete stored state. It returns
// the revision and the number of stored membership history rows.
func (p *PostgresPersister) readState(ctx context.Context, tx pgx.Tx, full bool) (State, uint64, int64, error) {
	var err error
	state := initialState()
	prepareInvestigations(&state)
	var revision int64
	var frameText []byte
	if err = tx.QueryRow(ctx, "SELECT revision, frame FROM "+p.table("meta")).Scan(&revision, &frameText); err != nil {
		return State{}, 0, 0, err
	}
	var frame pgFrame
	if err = json.Unmarshal(frameText, &frame); err != nil {
		return State{}, 0, 0, fmt.Errorf("meta frame: %w", err)
	}
	state.IssueAccessSequence, state.ReplayClock, state.LastClaimedProject, state.SchemaVersion, state.StoredFields = frame.IssueAccessSequence, frame.ReplayClock, frame.LastClaimedProject, frame.SchemaVersion, frame.Unknown
	readRows := func(table pgTable, query string, args ...any) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var payload []byte
			if err = rows.Scan(&id, &payload); err != nil {
				return err
			}
			if err = table.decode(&state, id, payload); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	var snapshots pgTable
	for _, table := range pgTables {
		if table.name == "snapshots" {
			snapshots = table
			continue
		}
		if err = readRows(table, "SELECT id, payload FROM "+p.table(table.name)); err != nil {
			return State{}, 0, 0, err
		}
	}
	if full {
		err = readRows(snapshots, "SELECT id, payload FROM "+p.table("snapshots"))
	} else {
		err = readRows(snapshots, "SELECT id, payload FROM "+p.table("snapshots")+" WHERE id = ANY($1)", hotSnapshotIDs(&state))
	}
	if err != nil {
		return State{}, 0, 0, err
	}
	for id, run := range state.Runs {
		if run.SnapshotID == "" || run.Input.ID != "" {
			continue
		}
		snap, ok := state.Snapshots[run.SnapshotID]
		if !ok {
			return State{}, 0, 0, fmt.Errorf("run %s: snapshot %s for its input is missing", id, run.SnapshotID)
		}
		run.Input = snap.Ticket
		state.Runs[id] = run
	}
	rows, err := tx.Query(ctx, "SELECT payload FROM "+p.table("case_history")+" ORDER BY seq")
	if err != nil {
		return State{}, 0, 0, err
	}
	for rows.Next() {
		var payload []byte
		var c Case
		if err = rows.Scan(&payload); err == nil {
			err = json.Unmarshal(payload, &c)
		}
		if err != nil {
			rows.Close()
			return State{}, 0, 0, fmt.Errorf("case history: %w", err)
		}
		state.CaseHistory = append(state.CaseHistory, c)
	}
	if err = rows.Err(); err != nil {
		return State{}, 0, 0, err
	}
	var membershipRows int64
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM "+p.table("membership_history")).Scan(&membershipRows); err != nil {
		return State{}, 0, 0, err
	}
	if full {
		if err = p.readColdRows(ctx, tx, &state); err != nil {
			return State{}, 0, 0, err
		}
	}
	return state, uint64(revision), membershipRows, nil
}

// readColdRows adds the membership history and raw event payloads that the
// working set does not load.
func (p *PostgresPersister) readColdRows(ctx context.Context, tx pgx.Tx, state *State) error {
	rows, err := tx.Query(ctx, "SELECT payload FROM "+p.table("membership_history")+" ORDER BY seq")
	if err != nil {
		return err
	}
	for rows.Next() {
		var payload []byte
		var link IssueTicketLink
		if err = rows.Scan(&payload); err == nil {
			err = json.Unmarshal(payload, &link)
		}
		if err != nil {
			rows.Close()
			return fmt.Errorf("membership history: %w", err)
		}
		state.MembershipHistory = append(state.MembershipHistory, link)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, "SELECT event_id, raw FROM "+p.table("event_raw"))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		r, ok := state.Events[id]
		if !ok {
			return fmt.Errorf("raw payload for unknown event %s", id)
		}
		r.Raw = json.RawMessage(raw)
		state.Events[id] = r
	}
	return rows.Err()
}

// hotSnapshotIDs lists the snapshots that code reads from memory: those of
// runs, investigation contracts and memberships that are not detached.
func hotSnapshotIDs(s *State) []string {
	hot := map[string]bool{}
	for _, run := range s.Runs {
		hot[run.SnapshotID] = true
	}
	for _, task := range s.Investigations {
		hot[task.Contract.SnapshotID] = true
	}
	for _, link := range s.Memberships {
		if link.State != "detached" {
			hot[link.SnapshotID] = true
		}
	}
	delete(hot, "")
	ids := make([]string, 0, len(hot))
	for id := range hot {
		ids = append(ids, id)
	}
	return ids
}

// Apply commits one Update in one transaction, fenced on the revision it was
// computed against and on this process's writer term.
func (p *PostgresPersister) Apply(c Commit) (uint64, error) {
	if p.term == 0 {
		return 0, errors.New("esa: Postgres Apply before AcquireWriter")
	}
	if c.Changes.CaseHistory.Rewritten || c.Changes.MembershipHistory.Rewritten {
		return 0, errors.New("esa: append-only history was rewritten")
	}
	ctx, cancel := p.withTimeout()
	defer cancel()
	next := int64(c.Revision) + 1
	frame, err := json.Marshal(pgFrame{IssueAccessSequence: c.State.IssueAccessSequence, ReplayClock: c.State.ReplayClock, LastClaimedProject: c.State.LastClaimedProject, SchemaVersion: c.State.SchemaVersion, Unknown: c.State.StoredFields})
	if err != nil {
		return 0, err
	}
	batch := &pgx.Batch{}
	for _, table := range pgTables {
		changes := c.Changes.Maps[table.name]
		for _, id := range changes.Upserted {
			payload, err := table.encode(&c.State, id)
			if err != nil {
				return 0, fmt.Errorf("%s %s: %w", table.name, id, err)
			}
			name := p.table(table.name)
			// Unchanged text is skipped, so an equal-content replacement never
			// trips the immutability triggers.
			batch.Queue("INSERT INTO "+name+" (id, payload, revision) VALUES ($1, $2::json, $3) ON CONFLICT (id) DO UPDATE SET payload = EXCLUDED.payload, revision = EXCLUDED.revision WHERE "+name+".payload::text IS DISTINCT FROM EXCLUDED.payload::text", id, string(payload), next)
			if table.name == "events" && len(c.State.Events[id].Raw) > 0 {
				batch.Queue("INSERT INTO "+p.table("event_raw")+" (event_id, raw) VALUES ($1, $2::json) ON CONFLICT (event_id) DO NOTHING", id, string(c.State.Events[id].Raw))
			}
		}
		if len(changes.Deleted) > 0 {
			batch.Queue("DELETE FROM "+p.table(table.name)+" WHERE id = ANY($1)", changes.Deleted)
		}
	}
	if err = queueHistory(p, batch, "case_history", "case_id", 0, c.State.CaseHistory, c.Changes.CaseHistory.Appended, func(c Case) string { return c.ID }); err != nil {
		return 0, err
	}
	if err = queueHistory(p, batch, "membership_history", "link_id", p.membershipBase, c.State.MembershipHistory, c.Changes.MembershipHistory.Appended, func(l IssueTicketLink) string { return l.ID }); err != nil {
		return 0, err
	}

	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, "UPDATE "+p.table("meta")+" SET revision = $1, frame = $2::json WHERE revision = $3 AND writer_term = $4", next, string(frame), int64(c.Revision), int64(p.term))
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, p.fenced(ctx)
	}
	if batch.Len() > 0 {
		if err = tx.SendBatch(ctx, batch).Close(); err != nil {
			return 0, err
		}
	}
	if p.testBeforeCommit != nil {
		if err = p.testBeforeCommit(); err != nil {
			return 0, err
		}
	}
	err = tx.Commit(ctx)
	if err == nil && p.testAfterCommit != nil {
		err = p.testAfterCommit()
	}
	if err != nil {
		return p.resolveCommit(c.Revision, err)
	}
	return uint64(next), nil
}

// queueHistory inserts the appended tail of an append-only history. seq is
// base plus the position in the in-memory slice; base counts rows that exist
// in the database but are not loaded.
func queueHistory[V any](p *PostgresPersister, batch *pgx.Batch, table, keyColumn string, base int64, history []V, appended int, key func(V) string) error {
	for i := len(history) - appended; i < len(history); i++ {
		payload, err := json.Marshal(history[i])
		if err != nil {
			return fmt.Errorf("%s %d: %w", table, i, err)
		}
		batch.Queue("INSERT INTO "+p.table(table)+" (seq, "+keyColumn+", payload) VALUES ($1, $2, $3::json)", base+int64(i), key(history[i]), string(payload))
	}
	return nil
}

// fenced explains a meta update that matched no row.
func (p *PostgresPersister) fenced(ctx context.Context) error {
	var revision, term int64
	if err := p.conn.QueryRow(ctx, "SELECT revision, writer_term FROM "+p.table("meta")).Scan(&revision, &term); err != nil {
		return fmt.Errorf("%w: %v", ErrLostWriter, err)
	}
	return fmt.Errorf("%w: database is at revision %d, term %d; this process holds term %d", ErrLostWriter, revision, term, p.term)
}

// resolveCommit settles a COMMIT that returned an error. A server error is a
// definite rollback. Otherwise the outcome is unknown, and meta.revision
// decides it: with a single writer, expected+1 means it committed.
func (p *PostgresPersister) resolveCommit(expected uint64, commitErr error) (uint64, error) {
	var pgErr *pgconn.PgError
	if errors.As(commitErr, &pgErr) {
		return 0, commitErr
	}
	ctx, cancel := p.withTimeout()
	defer cancel()
	if p.conn.IsClosed() {
		conn, err := pgx.Connect(ctx, p.url)
		if err != nil {
			return 0, fmt.Errorf("%w: commit outcome unknown: %v", ErrLostWriter, commitErr)
		}
		p.conn = conn
	}
	var revision, term int64
	if err := p.conn.QueryRow(ctx, "SELECT revision, writer_term FROM "+p.table("meta")).Scan(&revision, &term); err != nil {
		return 0, fmt.Errorf("%w: commit outcome unknown: %v", ErrLostWriter, commitErr)
	}
	switch {
	case uint64(term) != p.term:
		return 0, fmt.Errorf("%w: term changed during commit", ErrLostWriter)
	case uint64(revision) == expected+1:
		return expected + 1, nil
	case uint64(revision) == expected:
		return 0, commitErr
	default:
		return 0, fmt.Errorf("%w: unexpected revision %d after commit of %d", ErrLostWriter, revision, expected+1)
	}
}

// OpenPostgresStore acquires the writer role (waiting as a standby while
// another process holds it), then loads the working set into a Store.
func OpenPostgresStore(ctx context.Context, p *PostgresPersister, poll time.Duration) (*Store, error) {
	if err := p.AcquireWriter(ctx, poll); err != nil {
		return nil, err
	}
	return newStore(p)
}
