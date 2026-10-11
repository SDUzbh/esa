package esa

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type API struct {
	Engine *Engine
	Token  string
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, ErrInvalid) {
		status = 400
	} else if errors.Is(err, ErrNotFound) {
		status = 404
	} else if errors.Is(err, ErrConflict) {
		status = 409
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 12<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: expected one JSON document", ErrInvalid)
	}
	return nil
}
func projectScope(r *http.Request) []string {
	ids := []string{}
	for _, id := range strings.Split(r.Header.Get("X-ESA-Project-IDs"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
func allowed(r *http.Request, project string) bool {
	for _, id := range projectScope(r) {
		if id == project {
			return true
		}
	}
	return false
}

func ticketAllowed(r *http.Request, id string) bool {
	ids, present := r.Header[http.CanonicalHeaderKey("X-ESA-Ticket-IDs")]
	if !present {
		return true
	}
	for _, value := range ids {
		for _, candidate := range strings.Split(value, ",") {
			if candidate == id {
				return true
			}
		}
	}
	return false
}

// Withhold a report if its case evidence is outside the user's issue visibility.
func visibleRun(r *http.Request, s State, run Run) Run {
	for _, m := range run.Matches {
		c, ok := s.Cases[m.CaseID]
		if !ok || !ticketAllowed(r, c.TicketID) || !allowed(r, c.ProjectID) {
			run.Matches = nil
			run.Report = Report{Summary: "Case evidence is outside your issue visibility. Ask an authorized engineer to review this analysis."}
			break
		}
	}
	return run
}

func (api *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "workflow_version": WorkflowVersion})
	})
	mux.HandleFunc("POST /v1/events", api.events)
	mux.HandleFunc("GET /v1/tickets", api.tickets)
	mux.HandleFunc("GET /v1/tickets/{id}", api.ticket)
	mux.HandleFunc("POST /v1/tickets/{id}/analyze", api.analyze)
	mux.HandleFunc("GET /v1/runs/{id}", api.run)
	mux.HandleFunc("POST /v1/runs/{id}/review", api.reviewRun)
	mux.HandleFunc("GET /v1/cases", api.cases)
	mux.HandleFunc("POST /v1/cases/{id}/review", api.reviewCase)
	mux.HandleFunc("POST /v1/import/preview", api.preview)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || api.Token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(api.Token)) != 1 {
				writeJSON(w, 401, map[string]string{"error": "Authentication required."})
				return
			}
			if len(projectScope(r)) == 0 {
				writeJSON(w, 403, map[string]string{"error": "An explicit authorized project scope is required."})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (api *API) events(w http.ResponseWriter, r *http.Request) {
	var event Event
	if err := decodeJSON(w, r, &event); err != nil {
		writeError(w, err)
		return
	}
	if !allowed(r, event.Ticket.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	receipt, err := api.Engine.Receive(event)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 202, receipt)
}

type TicketRow struct {
	Ticket    Ticket `json:"ticket"`
	LatestRun *Run   `json:"latest_run,omitempty"`
	Case      *Case  `json:"case,omitempty"`
}

func rowFor(s State, t Ticket) TicketRow {
	row := TicketRow{Ticket: t}
	if run, ok := s.Runs[s.Latest[t.ID]]; ok {
		row.LatestRun = &run
	}
	for _, c := range s.Cases {
		if c.TicketID == t.ID {
			row.Case = &c
			break
		}
	}
	return row
}
func queryMatches(actual, wanted string) bool {
	return wanted == "" || contains(strings.Split(wanted, ","), actual)
}

func (api *API) tickets(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	q := r.URL.Query()
	rows := []TicketRow{}
	for _, t := range s.Tickets {
		if t.Private || !ticketAllowed(r, t.ID) || !allowed(r, t.ProjectID) || !queryMatches(t.ProjectID, q.Get("project_id")) || !queryMatches(t.Tracker, q.Get("tracker")) || !queryMatches(t.Status, q.Get("status")) || !queryMatches(t.Assignee, q.Get("assignee")) || !queryMatches(t.Source, q.Get("source")) {
			continue
		}
		row := rowFor(s, t)
		if row.LatestRun != nil {
			v := visibleRun(r, s, *row.LatestRun)
			row.LatestRun = &v
		}
		state, category, route := "", "", "case_collection"
		updated := t.UpdatedAt
		if row.LatestRun != nil && !t.Closed {
			state = row.LatestRun.State
			category = row.LatestRun.Classification.Category
			route = row.LatestRun.Route
			updated = row.LatestRun.UpdatedAt
		} else if row.Case != nil {
			state = row.Case.State
			category = row.Case.Category
			updated = row.Case.UpdatedAt
		}
		if !queryMatches(state, q.Get("esa_state")) || !queryMatches(category, q.Get("category")) || !queryMatches(route, q.Get("route")) {
			continue
		}
		if search := lower(q.Get("q")); search != "" && !strings.Contains(lower(t.ID+" "+t.SourceKey+" "+t.Subject), search) {
			continue
		}
		valid := true
		for _, bound := range []string{"updated_from", "updated_to"} {
			if value := q.Get(bound); value != "" {
				at, err := time.Parse(time.RFC3339, value)
				if err != nil {
					writeError(w, fmt.Errorf("%w: time filters require RFC3339", ErrInvalid))
					return
				}
				if bound == "updated_from" && updated.Before(at) || bound == "updated_to" && updated.After(at) {
					valid = false
				}
			}
		}
		if valid {
			rows = append(rows, row)
		}
	}
	sortKey := q.Get("sort")
	if sortKey == "" {
		sortKey = "updated_at"
	}
	if !contains([]string{"id", "subject", "project", "tracker", "status", "esa_state", "assignee", "source", "updated_at"}, sortKey) {
		writeError(w, fmt.Errorf("%w: unsupported sort column", ErrInvalid))
		return
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		writeError(w, fmt.Errorf("%w: direction must be asc or desc", ErrInvalid))
		return
	}
	key := func(row TicketRow) string {
		t := row.Ticket
		switch sortKey {
		case "id":
			return t.ID
		case "subject":
			return lower(t.Subject)
		case "project":
			return lower(t.Project)
		case "tracker":
			return lower(t.Tracker)
		case "status":
			return lower(t.Status)
		case "assignee":
			return lower(t.Assignee)
		case "source":
			return t.Source
		case "esa_state":
			if row.LatestRun != nil && !t.Closed {
				return row.LatestRun.State
			}
			if row.Case != nil {
				return row.Case.State
			}
		default:
			at := t.UpdatedAt
			if row.LatestRun != nil && !t.Closed {
				at = row.LatestRun.UpdatedAt
			} else if row.Case != nil {
				at = row.Case.UpdatedAt
			}
			return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
		}
		return ""
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := key(rows[i]), key(rows[j])
		if a == b {
			return rows[i].Ticket.ID < rows[j].Ticket.ID
		}
		if direction == "asc" {
			return a < b
		}
		return a > b
	})
	page, limit := 1, 25
	for name, dest := range map[string]*int{"page": &page, "limit": &limit} {
		if value := q.Get(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100000 {
				writeError(w, fmt.Errorf("%w: invalid pagination", ErrInvalid))
				return
			}
			*dest = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	total := len(rows)
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	writeJSON(w, 200, map[string]any{"tickets": rows[start:end], "total_count": total, "page": page, "limit": limit})
}

func (api *API) ticket(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	t, ok := s.Tickets[r.PathValue("id")]
	if !ok || !ticketAllowed(r, t.ID) || !allowed(r, t.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	history := []Run{}
	for _, run := range s.Runs {
		if run.TicketID == t.ID {
			history = append(history, visibleRun(r, s, run))
		}
	}
	sort.Slice(history, func(i, j int) bool { return history[i].CreatedAt.After(history[j].CreatedAt) })
	row := rowFor(s, t)
	if row.LatestRun != nil {
		v := visibleRun(r, s, *row.LatestRun)
		row.LatestRun = &v
	}
	writeJSON(w, 200, map[string]any{"ticket": row, "history": history})
}

func (api *API) analyze(w http.ResponseWriter, r *http.Request) {
	t, ok := api.Engine.Store.Snapshot().Tickets[r.PathValue("id")]
	if !ok || !ticketAllowed(r, t.ID) || !allowed(r, t.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	var input struct {
		Force bool `json:"force"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	receipt, err := api.Engine.AnalyzeTicket(t.ID, input.Force)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 202, receipt)
}

func (api *API) run(w http.ResponseWriter, r *http.Request) {
	run, ok := api.Engine.Store.Snapshot().Runs[r.PathValue("id")]
	if !ok || !ticketAllowed(r, run.TicketID) || !allowed(r, run.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	writeJSON(w, 200, visibleRun(r, api.Engine.Store.Snapshot(), run))
}
func (api *API) cases(w http.ResponseWriter, r *http.Request) {
	rows := []Case{}
	for _, c := range api.Engine.Store.Snapshot().Cases {
		if ticketAllowed(r, c.TicketID) && allowed(r, c.ProjectID) && queryMatches(c.ProjectID, r.URL.Query().Get("project_id")) && queryMatches(c.State, r.URL.Query().Get("state")) {
			rows = append(rows, c)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	writeJSON(w, 200, map[string]any{"cases": rows, "total_count": len(rows)})
}
func (api *API) reviewCase(w http.ResponseWriter, r *http.Request) {
	c, ok := api.Engine.Store.Snapshot().Cases[r.PathValue("id")]
	if !ok || !ticketAllowed(r, c.TicketID) || !allowed(r, c.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	var review Review
	if err := decodeJSON(w, r, &review); err != nil {
		writeError(w, err)
		return
	}
	result, err := api.Engine.ReviewCase(c.ID, review)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (api *API) reviewRun(w http.ResponseWriter, r *http.Request) {
	run, ok := api.Engine.Store.Snapshot().Runs[r.PathValue("id")]
	if !ok || !ticketAllowed(r, run.TicketID) || !allowed(r, run.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	var review Review
	if err := decodeJSON(w, r, &review); err != nil {
		writeError(w, err)
		return
	}
	result, err := api.Engine.ReviewRun(run.ID, review)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (api *API) preview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Filename  string `json:"filename"`
		ProjectID string `json:"project_id"`
		Content   string `json:"content"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	if !allowed(r, input.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	result, err := ParseImport(input.Filename, input.ProjectID, []byte(input.Content), api.Engine.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
