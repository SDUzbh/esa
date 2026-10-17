package esa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// richStateFile builds a JSON store that exercises every kind of row: open,
// analyzed and closed tickets, an ignored stale event, a raw payload, a
// rejection, membership and case history, and an unknown top-level field.
func richStateFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	e := testEngine(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e.Store = store
	analyzedTicket(t, e, "analyzed")
	open := forward(e, "open-event", fixture(e, "open"))
	open.Raw = json.RawMessage(`{"from":"jira","n":1}`)
	if _, err = e.Receive(open); err != nil {
		t.Fatal(err)
	}
	stale := fixture(e, "open")
	stale.Subject, stale.UpdatedAt = "older", stale.UpdatedAt.Add(-time.Hour)
	if _, err = e.Receive(forward(e, "stale-event", stale)); err != nil {
		t.Fatal(err)
	}
	closed := fixture(e, "closed")
	closed.Closed, closed.Status = true, "Resolved"
	closed.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
	receive(t, e, "closed-event", closed)
	if err = e.recordRejection(forward(e, "rejected-event", fixture(e, "other")), errors.New("ticket belongs to a different project")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]json.RawMessage
	if err = json.Unmarshal(b, &frame); err != nil {
		t.Fatal(err)
	}
	frame["future_field"] = json.RawMessage(`{"kept":true}`)
	if b, err = json.Marshal(frame); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func acquiredTestPostgres(t *testing.T, url, schema string) *PostgresPersister {
	t.Helper()
	p := openTestPostgres(t, url, schema)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.AcquireWriter(ctx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPostgresImportVerifiesAndPreservesTheSource(t *testing.T) {
	url := testDatabaseURL(t)
	schema := testSchema(t, url)
	path := richStateFile(t)
	original, _ := os.ReadFile(path)
	source, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p := acquiredTestPostgres(t, url, schema)
	if err = p.Import(context.Background(), source.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, original) {
		t.Fatal("import modified the source file")
	}
	store, err := newStore(p)
	if err != nil {
		t.Fatal(err)
	}
	want, got := source.Snapshot(), store.Snapshot()
	if err = sameStateEncoding("working set", hotProjection(want), hotProjection(got)); err != nil {
		t.Fatal(err)
	}
	if string(got.StoredFields["future_field"]) != `{"kept":true}` || got.Rejections["rejected-event"].TicketID != "other" || got.Events["stale-event"].Ticket == nil {
		t.Fatal("imported state lost unknown fields, rejections or a stale event's ticket")
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+pgx.Identifier{schema, "event_raw"}.Sanitize()+" WHERE raw::text = $1", `{"from":"jira","n":1}`); n != 1 {
		t.Fatal("raw payload not imported verbatim")
	}
	// The imported store keeps working: idempotency and new writes.
	e := testEngine(t)
	e.Store = store
	if receipt, err := e.Receive(forward(e, "open-event", fixture(e, "open"))); err != nil || receipt.Action != "duplicate" {
		t.Fatal("imported events no longer deduplicate", receipt.Action, err)
	}
	receive(t, e, "after-import", fixture(e, "new"))
	requireReloadMatches(t, p, e.Store.Snapshot())
}

func TestPostgresImportConvertsLegacyEvents(t *testing.T) {
	url := testDatabaseURL(t)
	e := testEngine(t)
	receive(t, e, "accepted", fixture(e, "T1"))
	legacy, err := OpenStore(legacyStateFile(t, e.Store.Snapshot(), ""))
	if err != nil {
		t.Fatal(err)
	}
	p := acquiredTestPostgres(t, url, testSchema(t, url))
	if err = p.Import(context.Background(), legacy.Snapshot()); err != nil {
		t.Fatal(err)
	}
	store, err := newStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if r := store.Snapshot().Events["accepted"]; r.Ticket != nil || r.SnapshotID == "" {
		t.Fatal("legacy event was imported with its ticket copy", r)
	}
}

func TestPostgresImportRefusesUsedDatabaseAndRollsBackOnMismatch(t *testing.T) {
	url := testDatabaseURL(t)
	schema := testSchema(t, url)
	source, err := OpenStore(richStateFile(t))
	if err != nil {
		t.Fatal(err)
	}
	p := acquiredTestPostgres(t, url, schema)
	p.testBeforeImportVerify = func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE "+p.table("tickets")+" SET payload = '{\"id\":\"tampered\"}' WHERE id = 'open'")
		return err
	}
	err = p.Import(context.Background(), source.Snapshot())
	if err == nil || !strings.Contains(err.Error(), `"tickets" differs`) {
		t.Fatal("tampered import was not rejected by verification", err)
	}
	if n := adminQueryInt(t, url, "SELECT count(*) FROM "+pgx.Identifier{schema, "tickets"}.Sanitize()); n != 0 {
		t.Fatal("failed verification left rows behind", n)
	}
	p.testBeforeImportVerify = nil
	if err = p.Import(context.Background(), source.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if err = p.Import(context.Background(), source.Snapshot()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "pristine") {
		t.Fatal("second import into a used database was accepted", err)
	}
}

func TestWriterGateStandbyActiveAndLost(t *testing.T) {
	var gate WriterGate
	get := func(path string) (int, string) {
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code, w.Body.String()
	}
	if code, body := get("/healthz"); code != 200 || !strings.Contains(body, `"role":"standby"`) {
		t.Fatal("standby health", code, body)
	}
	for _, path := range []string{"/healthz?role=active", "/v1/tickets"} {
		if code, body := get(path); code != 503 || !strings.Contains(body, "standby") {
			t.Fatal("standby must refuse", path, code, body)
		}
	}
	gate.Activate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	if code, _ := get("/healthz?role=active"); code != 299 {
		t.Fatal("active gate did not delegate", code)
	}
	gate.Lose()
	for _, path := range []string{"/healthz", "/v1/tickets"} {
		if code, _ := get(path); code != 503 {
			t.Fatal("lost gate still serving", path, code)
		}
	}
}

// One writer serves, a standby waits; when the writer loses its lock it goes
// unavailable, the standby takes over, and an outbox event the fenced writer
// could not commit is received and acknowledged exactly once by the new one.
func TestPostgresFailoverAcrossTwoWriters(t *testing.T) {
	url := testDatabaseURL(t)
	schema := testSchema(t, url)
	start := func() (*Engine, *PostgresPersister, *WriterGate) {
		p := openTestPostgres(t, url, schema)
		gate := &WriterGate{}
		e := testEngine(t)
		return e, p, gate
	}
	a, pa, gateA := start()
	a.Store = openTestPostgresStore(t, pa)
	gateA.Activate((&API{Engine: a, Token: strings.Repeat("t", 24)}).Handler())
	lost := make(chan error, 2)
	lose := func(err error) { gateA.Lose(); lost <- err }
	a.Store.OnLostWriter(lose)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pa.WatchLock(ctx, 20*time.Millisecond, lose)

	b, pb, gateB := start()
	ready := make(chan struct{})
	go func() {
		store, err := OpenPostgresStore(ctx, pb, 20*time.Millisecond)
		if err != nil {
			t.Error(err)
			return
		}
		b.Store = store
		gateB.Activate((&API{Engine: b, Token: strings.Repeat("t", 24)}).Handler())
		close(ready)
	}()
	status := func(g *WriterGate, path string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 24))
		r.Header.Set("X-ESA-Project-IDs", "1")
		g.ServeHTTP(w, r)
		return w.Code
	}
	if status(gateB, "/healthz") != 200 || status(gateB, "/healthz?role=active") != 503 || status(gateB, "/v1/tickets") != 503 {
		t.Fatal("standby endpoints wrong before takeover")
	}
	if status(gateA, "/v1/tickets") != 200 {
		t.Fatal("writer not serving")
	}

	adminExec(t, url, "SELECT pg_terminate_backend($1)", lockPID(t, pa))
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("standby never took over")
	}
	outbox := &fakeOutbox{events: []Event{outboxEvent(a, "redmine_event_1", fixture(a, "T1"))}}
	srv := outbox.serve(t)
	if err := a.SyncRedmine(context.Background(), srv.URL, "token", srv.Client()); !errors.Is(err, ErrLostWriter) {
		t.Fatal("fenced writer did not fail the outbox event", err)
	}
	if len(outbox.acked) != 0 {
		t.Fatal("fenced writer acknowledged an event it could not commit", outbox.acked)
	}
	select {
	case err := <-lost:
		if !errors.Is(err, ErrLostWriter) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old writer never noticed it lost the writer role")
	}
	if status(gateA, "/v1/tickets") != 503 || status(gateA, "/healthz") != 503 {
		t.Fatal("old writer still serving after losing the role")
	}
	if err := b.SyncRedmine(context.Background(), srv.URL, "token", srv.Client()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(outbox.acked, ",") != "redmine_event_1" || b.Store.Snapshot().Tickets["T1"].ID != "T1" {
		t.Fatal("new writer did not receive the event exactly once", outbox.acked)
	}
	if status(gateB, "/v1/tickets") != 200 || status(gateB, "/healthz?role=active") != 200 {
		t.Fatal("new writer not serving")
	}
}
