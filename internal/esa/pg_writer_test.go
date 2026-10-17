package esa

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
	if strings.Join(outbox.acked, ",") != "redmine_event_1" || b.Store.Snapshot().Tickets.Get("T1").ID != "T1" {
		t.Fatal("new writer did not receive the event exactly once", outbox.acked)
	}
	if status(gateB, "/v1/tickets") != 200 || status(gateB, "/healthz?role=active") != 200 {
		t.Fatal("new writer not serving")
	}
}
