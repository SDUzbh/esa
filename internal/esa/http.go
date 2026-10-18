package esa

import (
	"context"
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

// API exposes the business boundary to trusted integrations with explicit project scopes.
// The Redmine plugin adds user/issue authorization before using the service credential.
// API serves ESA's HTTP interface. Token is the Redmine plugin's secret
// (ESA_API_TOKEN): its requests must carry a ticket scope and cannot ingest
// events or move the replay clock. OperatorToken (ESA_OPERATOR_TOKEN, optional)
// is for the replay harness and local scripts: unscoped reads, event ingestion
// and the replay clock. An empty OperatorToken closes those to everyone.
type API struct {
	Engine        *Engine
	Token         string
	OperatorToken string
}

// operatorRoutes are reserved for the operator token.
var operatorRoutes = map[string]bool{"POST /v1/events": true, "POST /v1/replay/clock": true}

// bearer reports whether the request presents token as a bearer credential.
func bearer(r *http.Request, token string) bool {
	header := r.Header.Get("Authorization")
	return token != "" && strings.HasPrefix(header, "Bearer ") && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(token)) == 1
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

// accessScope is a request's parsed X-ESA-Project-IDs and X-ESA-Ticket-IDs.
// tickets is nil when the request has no ticket scope, which allows every
// ticket. Handler parses it once per request: list endpoints check it for
// each row, and the plugin sends up to 2,000 ticket IDs.
type accessScope struct {
	projects map[string]bool
	tickets  map[string]bool
}

type accessScopeKey struct{}

func parseAccessScope(r *http.Request) accessScope {
	scope := accessScope{projects: map[string]bool{}}
	for _, id := range strings.Split(r.Header.Get("X-ESA-Project-IDs"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			scope.projects[id] = true
		}
	}
	if values, present := r.Header[http.CanonicalHeaderKey("X-ESA-Ticket-IDs")]; present {
		scope.tickets = map[string]bool{}
		for _, value := range values {
			for _, id := range strings.Split(value, ",") {
				scope.tickets[id] = true
			}
		}
	}
	return scope
}

func requestScope(r *http.Request) accessScope {
	if scope, ok := r.Context().Value(accessScopeKey{}).(accessScope); ok {
		return scope
	}
	return parseAccessScope(r)
}

func allowed(r *http.Request, project string) bool {
	return requestScope(r).projects[project]
}

func ticketAllowed(r *http.Request, id string) bool {
	tickets := requestScope(r).tickets
	return tickets == nil || tickets[id]
}

// Withhold a report if its case evidence is outside the user's issue visibility.
func visibleRun(r *http.Request, s State, run Run) Run {
	// Retired runs without explicit provenance cannot establish visibility of
	// their former cross-ticket evidence. Preserve them only in the stored audit.
	if run.WorkflowVersion != WorkflowVersion && run.InvestigationID == "" && run.SourceTicketID == "" {
		run.Report = Report{Summary: "Historical evidence has no verifiable source provenance; inspect the retained audit with an authorized engineer."}
	}
	run.StoredFields = nil
	run.Report.StoredFields = nil
	run.Report.AccessedIssueIDs = nil
	run.Input.StoredFields = nil
	// The frozen provider request is internal resume state, not public output.
	run.PipelineInput = nil
	if run.SourceTicketID != "" {
		source, ok := s.Tickets.Lookup(run.SourceTicketID)
		if !ok || source.Private || !ticketAllowed(r, run.SourceTicketID) || !allowed(r, source.ProjectID) {
			run.SourceTicketID = ""
			run.Report = Report{Summary: "Shared investigation evidence is outside your issue visibility. Ask an authorized engineer to review it."}
		}
	}
	return run
}

// Handler authenticates non-health requests and refreshes authoritative state.
// Project scope is mandatory; optional ticket scopes narrow visible source evidence.
func (api *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "workflow_version": api.Engine.Version()})
	})
	mux.HandleFunc("POST /v1/events", api.events)
	mux.HandleFunc("GET /v1/tickets", api.tickets)
	mux.HandleFunc("GET /v1/tickets/{id}", api.ticket)
	mux.HandleFunc("POST /v1/tickets/{id}/analyze", api.analyze)
	mux.HandleFunc("GET /v1/tickets/{id}/classifier", api.classifierStatus)
	mux.HandleFunc("POST /v1/tickets/{id}/classifier/predict", api.classifierPredict)
	mux.HandleFunc("POST /v1/tickets/{id}/classifier/feedback", api.classifierFeedback)
	mux.HandleFunc("GET /v1/classifier/feedback", api.classifierExport)
	mux.HandleFunc("GET /v1/runs/{id}", api.run)
	mux.HandleFunc("GET /v1/cases", api.cases)
	mux.HandleFunc("POST /v1/cases/{id}/review", api.reviewCase)
	mux.HandleFunc("POST /v1/import/preview", api.preview)
	mux.HandleFunc("GET /v1/engineering-issues", api.engineeringIssues)
	mux.HandleFunc("POST /v1/tickets/{id}/membership/review", api.reviewMembership)
	mux.HandleFunc("GET /v1/investigations/{id}", api.investigation)
	mux.HandleFunc("GET /v1/investigations/{id}/solutions", api.solutionHistory)
	mux.HandleFunc("POST /v1/investigations/{id}/review", api.reviewSolution)
	mux.HandleFunc("GET /v1/snapshots/{id}", api.snapshot)
	mux.HandleFunc("POST /v1/replay/clock", api.advanceClock)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeError(w, ErrNotFound) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" {
			operator := bearer(r, api.OperatorToken)
			if !operator && !bearer(r, api.Token) {
				writeJSON(w, 401, map[string]string{"error": "Authentication required."})
				return
			}
			scope := parseAccessScope(r)
			if !operator && operatorRoutes[r.Method+" "+r.URL.Path] {
				writeJSON(w, 403, map[string]string{"error": "This route requires the operator token."})
				return
			}
			if !operator && scope.tickets == nil {
				writeJSON(w, 403, map[string]string{"error": "An explicit ticket scope (X-ESA-Ticket-IDs) is required."})
				return
			}
			if len(scope.projects) == 0 {
				writeJSON(w, 403, map[string]string{"error": "An explicit authorized project scope is required."})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), accessScopeKey{}, scope))
			if err := api.Engine.Refresh(); err != nil {
				writeJSON(w, 503, map[string]string{"error": "Authoritative ESA state is unavailable."})
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
	Application      *SolutionApplication `json:"solution_application,omitempty"`
	Membership       *IssueTicketLink     `json:"membership,omitempty"`
	SnapshotID       string               `json:"snapshot_id,omitempty"`
	Investigation    *Investigation       `json:"investigation,omitempty"`
	Solution         *SolutionRevision    `json:"solution,omitempty"`
	EngineeringIssue *EngineeringIssue    `json:"engineering_issue,omitempty"`
	Ticket           Ticket               `json:"ticket"`
	LatestRun        *Run                 `json:"latest_run,omitempty"`
	Case             *Case                `json:"case,omitempty"`
}

func rowFor(s State, t Ticket) TicketRow {
	row := TicketRow{Ticket: t, SnapshotID: s.LatestSnapshot.Get(t.ID)}
	if run, ok := s.Runs.Lookup(s.Latest.Get(t.ID)); ok {
		run = runForTicket(run, t)
		row.LatestRun = &run
		if task, ok := s.Investigations.Lookup(run.InvestigationID); ok {
			row.Investigation = &task
			row.Application = task.Application
			if solution, ok := taskSolution(s, task); ok {
				row.Solution = &solution
			}
			if issue, ok := s.Issues.Lookup(task.IssueID); ok {
				row.EngineeringIssue = &issue
			}
		}
	}
	if link, ok := activeMembership(s, t.ID); ok {
		row.Membership = &link
		if issue, exists := s.Issues.Lookup(link.IssueID); exists {
			row.EngineeringIssue = &issue
		}
		if row.Investigation != nil && link.IssueID != row.Investigation.IssueID {
			row.Investigation = nil
			row.Solution = nil
			row.Application = nil
			if row.LatestRun != nil {
				copy := *row.LatestRun
				copy.IssueID = link.IssueID
				copy.State = RunMembershipReviewed
				copy.Report = Report{Summary: "Membership updated without analysis. The previous diagnosis remains in history; request analysis explicitly if needed."}
				row.LatestRun = &copy
			}
		}
	}
	if ids := s.casesOfTicket(t.ID); len(ids) > 0 {
		c := s.Cases.Get(ids[0])
		row.Case = &c
	}
	if row.Investigation != nil && row.Investigation.IssueID != "" {
		if _, exists := s.Issues.Lookup(row.Investigation.IssueID); !exists {
			row.Investigation, row.Solution, row.Application = nil, nil, nil
			if row.LatestRun != nil {
				copy := *row.LatestRun
				copy.State = RunIssueRemoved
				copy.Report = Report{Summary: "The associated engineering issue was removed by retention. Earlier evidence remains in history; explicitly forward the ticket to analyze again."}
				row.LatestRun = &copy
			}
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
	var bounds [2]time.Time
	for i, bound := range []string{"updated_from", "updated_to"} {
		if value := q.Get(bound); value != "" {
			at, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(w, fmt.Errorf("%w: time filters require RFC3339", ErrInvalid))
				return
			}
			bounds[i] = at
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
	key := func(t Ticket, state string, updated time.Time) string {
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
			return state
		}
		return updated.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	// A ticket-scoped request (every plugin request) visits only its scoped
	// tickets, so the page costs O(scope), not O(archive).
	candidates := s.Tickets.All()
	if scoped := requestScope(r).tickets; scoped != nil {
		candidates = func(yield func(string, Ticket) bool) {
			for id := range scoped {
				if t, ok := s.Tickets.Lookup(id); ok && !yield(id, t) {
					return
				}
			}
		}
	}
	type sortedRow struct {
		row TicketRow
		key string
	}
	rows := []sortedRow{}
	for _, t := range candidates {
		if t.ForwardedBy == "" || t.ForwardedAt.IsZero() || t.Private || !ticketAllowed(r, t.ID) || !allowed(r, t.ProjectID) || !queryMatches(t.ProjectID, q.Get("project_id")) || !queryMatches(t.Tracker, q.Get("tracker")) || !queryMatches(t.Status, q.Get("status")) || !queryMatches(t.Assignee, q.Get("assignee")) || !queryMatches(t.Source, q.Get("source")) {
			continue
		}
		row := rowFor(s, t)
		if row.LatestRun != nil {
			v := visibleRun(r, s, *row.LatestRun)
			row.LatestRun = &v
		}
		row = visibleRow(r, s, row, api.Engine.Now())
		state, route := "", "case_collection"
		updated := t.UpdatedAt
		if row.LatestRun != nil && !t.Closed {
			state = string(row.LatestRun.State)
			route = row.LatestRun.Route
			updated = row.LatestRun.UpdatedAt
		} else if row.Case != nil {
			state = string(row.Case.State)
			updated = row.Case.UpdatedAt
		}
		if !queryMatches(state, q.Get("esa_state")) || !queryMatches(route, q.Get("route")) {
			continue
		}
		if search := lower(q.Get("q")); search != "" && !strings.Contains(lower(t.ID+" "+t.SourceKey+" "+t.Subject), search) {
			continue
		}
		if !bounds[0].IsZero() && updated.Before(bounds[0]) || !bounds[1].IsZero() && updated.After(bounds[1]) {
			continue
		}
		rows = append(rows, sortedRow{row, key(t, state, updated)})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].key, rows[j].key
		if a == b {
			return rows[i].row.Ticket.ID < rows[j].row.Ticket.ID
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
	pageRows := make([]TicketRow, 0, end-start)
	for _, row := range rows[start:end] {
		pageRows = append(pageRows, row.row)
	}
	writeJSON(w, 200, map[string]any{"tickets": pageRows, "total_count": total, "page": page, "limit": limit})
}

func (api *API) ticket(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	t, ok := s.Tickets.Lookup(r.PathValue("id"))
	if !ok || t.ForwardedBy == "" || t.ForwardedAt.IsZero() || !ticketAllowed(r, t.ID) || !allowed(r, t.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	// Record the access only when it would advance an Issue's LRU order; a write
	// is a database transaction, so plain reads must not pay for one.
	if !t.Private && ticketTouchTarget(s, t.ID, api.Engine.Now()) != "" {
		if err := api.Engine.update(func(state *State) error {
			current := state.Tickets.Get(t.ID)
			if !current.Private && ticketAllowed(r, current.ID) && allowed(r, current.ProjectID) {
				touchTicketIssue(state, current.ID, api.Engine.Now())
			}
			return nil
		}); err != nil {
			writeError(w, err)
			return
		}
		s = api.Engine.Store.Snapshot()
		t = s.Tickets.Get(t.ID)
	}
	history := []Run{}
	seen := map[string]bool{}
	for _, id := range s.runsOfTicket(t.ID) {
		run := s.Runs.Get(id)
		history = append(history, visibleRun(r, s, run))
		seen[run.ID] = true
	}
	for _, id := range s.membershipsOfTicket(t.ID) {
		link := s.Memberships.Get(id)
		task := s.Investigations.Get(link.InvestigationID)
		run, ok := s.Runs.Lookup(task.RunID)
		if ok && !seen[run.ID] {
			history = append(history, visibleRun(r, s, runForTicket(run, t)))
			seen[run.ID] = true
		}
	}
	sort.Slice(history, func(i, j int) bool { return history[i].CreatedAt.After(history[j].CreatedAt) })
	row := rowFor(s, t)
	if row.LatestRun != nil {
		v := visibleRun(r, s, *row.LatestRun)
		row.LatestRun = &v
	}
	row = visibleRow(r, s, row, api.Engine.Now())
	result := map[string]any{"ticket": row, "history": history}
	if api.Engine.Config.Classifier.Enabled && !t.Private {
		result["classifier"] = api.Engine.classifierView(s, t)
	}
	writeJSON(w, 200, result)
}

func (api *API) analyze(w http.ResponseWriter, r *http.Request) {
	t, ok := api.Engine.Store.Snapshot().Tickets.Lookup(r.PathValue("id"))
	if !ok || t.ForwardedBy == "" || t.ForwardedAt.IsZero() || !ticketAllowed(r, t.ID) || !allowed(r, t.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	var input struct {
		Force       bool   `json:"force"`
		ForwardedBy string `json:"forwarded_by"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	receipt, err := api.Engine.AnalyzeTicket(t.ID, input.Force, input.ForwardedBy)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 202, receipt)
}

func (api *API) run(w http.ResponseWriter, r *http.Request) {
	run, ok := api.Engine.Store.Snapshot().Runs.Lookup(r.PathValue("id"))
	if !ok || !ticketAllowed(r, run.TicketID) || !allowed(r, run.ProjectID) {
		writeError(w, ErrNotFound)
		return
	}
	writeJSON(w, 200, visibleRun(r, api.Engine.Store.Snapshot(), run))
}
func (api *API) cases(w http.ResponseWriter, r *http.Request) {
	rows := []Case{}
	for _, c := range api.Engine.Store.Snapshot().Cases.All() {
		if ticketAllowed(r, c.TicketID) && allowed(r, c.ProjectID) && queryMatches(c.ProjectID, r.URL.Query().Get("project_id")) && queryMatches(string(c.State), r.URL.Query().Get("state")) {
			rows = append(rows, c)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	writeJSON(w, 200, map[string]any{"cases": rows, "total_count": len(rows)})
}
func (api *API) reviewCase(w http.ResponseWriter, r *http.Request) {
	c, ok := api.Engine.Store.Snapshot().Cases.Lookup(r.PathValue("id"))
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
