package codex

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"esa/internal/esa"
)

// Handler is an operator-only API. Raw events may contain private ticket/code
// content; the service credential must never be exposed to a browser.
// Handler serves authenticated stage requests and bounded raw event-log pages.
// Logs are operator data, not browser-accessible synthesized reasoning traces.
func (p *Provider) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":            "ok",
			"provider":          "codex_cli",
			"registered_source": p.Packet.Ticket.SourceKey,
			"model":             p.Model,
		})
	})
	mux.HandleFunc("POST /analyze", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		var in esa.AnalyzeRequest
		if err := d.Decode(&in); err != nil {
			http.Error(w, "Invalid provider input", 400)
			return
		}
		report, err := p.Analyze(r.Context(), in)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			slog.Warn("Codex diagnosis failed", "run_id", in.RunID, "error", err)
			w.WriteHeader(422)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "report": report})
			return
		}
		_ = json.NewEncoder(w).Encode(report)
	})
	mux.HandleFunc("GET /runs/{id}/logs", p.runtimeLogs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" {
			header := r.Header.Get("Authorization")
			if token == "" ||
				!strings.HasPrefix(header, "Bearer ") ||
				subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(token)) != 1 {
				http.Error(w, "Authentication required", 401)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (p *Provider) runtimeLogs(w http.ResponseWriter, r *http.Request) {
	id, stage := r.PathValue("id"), r.URL.Query().Get("stage")
	paths := map[string]string{
		"route":    filepath.Join(id, "routing", "events.jsonl"),
		"engineer": filepath.Join(id, "engineering", id, "events.jsonl"),
		"assign":   filepath.Join(id, "assignment", "events.jsonl"),
	}
	path, ok := paths[stage]
	if !validRunID(id) || !ok {
		http.Error(w, "Invalid run or stage (route, engineer, assign)", 400)
		return
	}
	var offset int64
	var err error
	if value := r.URL.Query().Get("offset"); value != "" {
		offset, err = strconv.ParseInt(value, 10, 64)
	}
	if err != nil || offset < 0 {
		http.Error(w, "Invalid log offset", 400)
		return
	}
	result := struct {
		RunID      string            `json:"run_id"`
		Stage      string            `json:"stage"`
		Available  bool              `json:"available"`
		NextOffset int64             `json:"next_offset"`
		HasMore    bool              `json:"has_more"`
		Events     []json.RawMessage `json:"events"`
	}{RunID: id, Stage: stage, NextOffset: offset, Events: []json.RawMessage{}}
	// Root confines fixed log paths even when a symlink is present on disk.
	root, err := os.OpenRoot(p.RunsDir)
	if errors.Is(err, os.ErrNotExist) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	if err != nil {
		http.Error(w, "Provider logs unavailable", 503)
		return
	}
	defer root.Close()
	file, err := root.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "Cannot read stage log", 500)
		return
	}
	if err == nil {
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.Error(w, "Invalid stage log", 500)
			return
		}
		if offset > info.Size() {
			http.Error(w, "Offset exceeds log size", 400)
			return
		}
		if offset > 0 {
			var last [1]byte
			if _, err := file.ReadAt(last[:], offset-1); err != nil || last[0] != '\n' {
				http.Error(w, "Offset must follow a complete event", 400)
				return
			}
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			http.Error(w, "Cannot seek stage log", 500)
			return
		}
		result.Available = true
		scanner := bufio.NewReader(io.LimitReader(file, 4<<20))
		for len(result.Events) < 200 && result.NextOffset-offset < 2<<20 {
			line, err := scanner.ReadBytes('\n')
			if err == io.EOF {
				break // Do not advance past a partially written event.
			}
			if err != nil || !json.Valid(line) {
				http.Error(w, "Invalid runtime event", 500)
				return
			}
			result.NextOffset += int64(len(line))
			result.Events = append(result.Events, json.RawMessage(line))
			result.HasMore = result.NextOffset < info.Size()
		}
		// A partial trailing line is not a complete page to drain immediately.
		if len(result.Events) < 200 && result.NextOffset-offset < 2<<20 {
			result.HasMore = false
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
