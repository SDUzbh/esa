package esa

import (
	"net/http"
	"sync/atomic"
)

// WriterGate fronts the HTTP API of a process that may not be the state
// writer. Until Activate it answers as a standby: /healthz returns 200 with
// role "standby" (so a supervisor keeps it running) but 503 for
// /healthz?role=active (so a proxy routes only to the writer), and every other
// route returns 503. After Lose, every route returns 503 while the process
// shuts down. Never put a writer and a standby behind one round-robin name.
type WriterGate struct {
	active atomic.Pointer[http.Handler]
	lost   atomic.Bool
}

// Activate starts serving h; call it once the store is loaded as the writer.
func (g *WriterGate) Activate(h http.Handler) { g.active.Store(&h) }

// Lose stops serving the API for good; the writer role is gone.
func (g *WriterGate) Lose() { g.lost.Store(true) }

func (g *WriterGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if g.lost.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ESA lost the state writer role and is stopping.", "role": "stopping"})
		return
	}
	if h := g.active.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	if r.URL.Path == "/healthz" && r.URL.Query().Get("role") != "active" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "role": "standby"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ESA standby: not the active writer.", "role": "standby"})
}
