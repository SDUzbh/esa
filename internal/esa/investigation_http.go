package esa

import (
	"fmt"
	"net/http"
	"sort"
	"time"
)

func runForTicket(run Run, t Ticket) Run {
	if run.InvestigationID != "" && run.TicketID != t.ID {
		if run.SourceTicketID == "" {
			run.SourceTicketID = run.TicketID
		}
		run.TicketID = t.ID
		run.Input = t
	}
	return run
}
func visibleRow(r *http.Request, s State, row TicketRow, at time.Time) TicketRow {
	if row.Membership != nil {
		run := s.Runs.Get(s.Investigations.Get(row.Membership.InvestigationID).RunID)
		sourceID := run.SourceTicketID
		if sourceID == "" && run.TicketID != row.Ticket.ID {
			sourceID = run.TicketID
		}
		if sourceID != "" {
			source, exists := s.Tickets.Lookup(sourceID)
			if !exists || source.Private || !allowed(r, source.ProjectID) || !ticketAllowed(r, sourceID) {
				copy := *row.Membership
				copy.Basis = "Grouping evidence is outside your source visibility."
				row.Membership = &copy
			}
		}
	}
	if row.Investigation != nil {
		task := visibleTask(r, s, *row.Investigation)
		row.Investigation = &task
		row.Application = task.Application
	}
	if row.EngineeringIssue != nil {
		copy := visibleEngineeringIssue(r, s, *row.EngineeringIssue)
		row.EngineeringIssue = &copy
	}
	applicationRevoked := row.Application != nil && (row.Application.State == "rejected" || row.Application.State == "withdrawn")
	if applicationRevoked && row.LatestRun != nil {
		copy := *row.LatestRun
		copy.Report = Report{Summary: "Solution applicability was rejected or withdrawn. Earlier evidence remains in the audit history."}
		row.LatestRun = &copy
	}
	if row.Solution != nil {
		sol := row.Solution
		copy := *sol
		copy.StoredFields = nil
		row.Solution = &copy
		source, ok := s.Tickets.Lookup(sol.SourceTicketID)
		if !ok || source.Private || !ticketAllowed(r, source.ID) || !allowed(r, source.ProjectID) || sol.AvailableAt.After(at) {
			row.Solution = nil
			row.Application = nil
		} else if sol.State == "rejected" || sol.State == "withdrawn" || applicationRevoked {
			copy.Resolution = Resolution{}
			report := Report{Summary: "The shared proposal was rejected or withdrawn. Earlier evidence remains in the audit history."}
			if row.LatestRun != nil {
				copyRun := *row.LatestRun
				copyRun.Report = report
				row.LatestRun = &copyRun
			}
		}
	}
	return row
}
func (api *API) reviewMembership(w http.ResponseWriter, r *http.Request) {
	t, ok := api.Engine.Store.Snapshot().Tickets.Lookup(r.PathValue("id"))
	if !ok || t.Private || !allowed(r, t.ProjectID) || !ticketAllowed(r, t.ID) {
		writeError(w, ErrNotFound)
		return
	}
	var review MembershipReview
	if err := decodeJSON(w, r, &review); err != nil {
		writeError(w, err)
		return
	}
	if review.IssueID != "" {
		visible := false
		state := api.Engine.Store.Snapshot()
		for _, link := range state.Memberships.All() {
			member, exists := state.Tickets.Lookup(link.TicketID)
			visible = visible || exists && !member.Private && member.ProjectID == t.ProjectID && link.IssueID == review.IssueID && link.State == "tentative" && ticketAllowed(r, link.TicketID)
		}
		if !visible {
			writeError(w, ErrNotFound)
			return
		}
	}
	receipt, err := api.Engine.ReviewMembership(t.ID, review)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}
func visibleTask(r *http.Request, s State, task Investigation) Investigation {
	if task.Application != nil {
		source, ok := s.Tickets.Lookup(task.Application.SourceTicketID)
		if !ok || source.Private || !ticketAllowed(r, source.ID) || !allowed(r, source.ProjectID) {
			task.Application = nil
		}
	}
	return task
}
func taskVisible(r *http.Request, s State, task Investigation) bool {
	if !allowed(r, task.ProjectID) {
		return false
	}
	if source, ok := s.Tickets.Lookup(s.Runs.Get(task.RunID).TicketID); ok && !source.Private && ticketAllowed(r, source.ID) {
		return true
	}
	for _, id := range s.membershipsOfInvestigation(task.ID) {
		link := s.Memberships.Get(id)
		t, ok := s.Tickets.Lookup(link.TicketID)
		if ok && link.State == "tentative" && !t.Private && ticketAllowed(r, t.ID) {
			return true
		}
	}
	return false
}
func (api *API) solutionHistory(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	task, ok := s.Investigations.Lookup(r.PathValue("id"))
	source := s.Tickets.Get(s.Runs.Get(task.RunID).TicketID)
	if !ok || source.Private || !allowed(r, task.ProjectID) || !ticketAllowed(r, source.ID) {
		writeError(w, ErrNotFound)
		return
	}
	rows := []SolutionRevision{}
	lineage := map[string]bool{}
	current := task.SolutionID
	if task.Application != nil {
		current = task.Application.SolutionID
	}
	for current != "" && !lineage[current] {
		lineage[current] = true
		current = s.Solutions.Get(current).Supersedes
	}
	for _, solution := range s.Solutions.All() {
		source, exists := s.Tickets.Lookup(solution.SourceTicketID)
		if (solution.InvestigationID == task.ID || lineage[solution.ID]) && !solution.AvailableAt.After(api.Engine.Now()) && exists && !source.Private && allowed(r, source.ProjectID) && ticketAllowed(r, source.ID) {
			solution.StoredFields = nil
			rows = append(rows, solution)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Revision == rows[j].Revision {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Revision < rows[j].Revision
	})
	writeJSON(w, 200, map[string]any{"solutions": rows, "solution_application": visibleTask(r, s, task).Application})
}

// Canonical titles never depend on the representative ticket. Hide a title if
// the viewer cannot see its provenance, including older records without it.
func issueTitleSource(s State, issue EngineeringIssue) string {
	source := issue.TitleSourceTicketID
	if source == "" {
		var first Run
		for _, run := range s.Runs.All() {
			if run.IssueID == issue.ID && (first.ID == "" || run.CreatedAt.Before(first.CreatedAt) || run.CreatedAt.Equal(first.CreatedAt) && run.ID < first.ID) {
				first = run
			}
		}
		source = first.TicketID
		if first.Report.Routing != nil && first.Report.Routing.Action == "reuse" && first.SourceTicketID != "" {
			source = first.SourceTicketID
		}
	}
	return source
}
func visibleEngineeringIssue(r *http.Request, s State, issue EngineeringIssue) EngineeringIssue {
	source := issueTitleSource(s, issue)
	t, ok := s.Tickets.Lookup(source)
	if !ok || t.Private || !ticketAllowed(r, source) || !allowed(r, t.ProjectID) {
		issue.Title = ""
		issue.TitleSourceTicketID = ""
		issue.TitleRestricted = true
	}
	return issue
}
func (api *API) engineeringIssues(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	rows := []map[string]any{}
	for _, issue := range s.Issues.All() {
		if !allowed(r, issue.ProjectID) {
			continue
		}
		ids := map[string]bool{}
		title := ""
		for _, link := range s.Memberships.All() {
			t, ok := s.Tickets.Lookup(link.TicketID)
			if link.IssueID == issue.ID && link.State == "tentative" && ok && !t.Private && ticketAllowed(r, t.ID) {
				ids[t.ID] = true
				if title == "" || t.ID < title {
					title = t.ID
				}
			}
		}
		if len(ids) == 0 {
			continue
		}
		members := []string{}
		for id := range ids {
			members = append(members, id)
		}
		sort.Strings(members)
		copy := visibleEngineeringIssue(r, s, issue)
		copy.State = issueState(s, issue.ID, ids)
		rows = append(rows, map[string]any{"issue": copy, "ticket_ids": members, "visible_ticket_count": len(members), "representative_ticket": map[string]string{"id": title, "subject": s.Tickets.Get(title).Subject}})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["issue"].(EngineeringIssue).ID < rows[j]["issue"].(EngineeringIssue).ID
	})
	writeJSON(w, 200, map[string]any{"issues": rows, "total_count": len(rows)})
}
func (api *API) investigation(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	task, ok := s.Investigations.Lookup(r.PathValue("id"))
	if !ok || !taskVisible(r, s, task) {
		writeError(w, ErrNotFound)
		return
	}
	if err := api.Engine.update(func(state *State) error {
		current := state.Investigations.Get(task.ID)
		if taskVisible(r, *state, current) {
			touchIssue(state, current.IssueID, api.Engine.Now())
		}
		return nil
	}); err != nil {
		writeError(w, err)
		return
	}
	s = api.Engine.Store.Snapshot()
	task = s.Investigations.Get(task.ID)
	view := visibleTask(r, s, task)
	result := map[string]any{"investigation": view, "solution_application": view.Application}
	source := s.Tickets.Get(s.Runs.Get(task.RunID).TicketID)
	if !source.Private && ticketAllowed(r, source.ID) {
		result["run"] = visibleRun(r, s, s.Runs.Get(task.RunID))
		if sol, ok := taskSolution(s, task); ok && !sol.AvailableAt.After(api.Engine.Now()) {
			evidenceSource, exists := s.Tickets.Lookup(sol.SourceTicketID)
			if exists && !evidenceSource.Private && allowed(r, evidenceSource.ProjectID) && ticketAllowed(r, evidenceSource.ID) {
				sol.StoredFields = nil
				result["solution"] = sol
			}
		}
	}
	writeJSON(w, 200, result)
}
func (api *API) reviewSolution(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	task, ok := s.Investigations.Lookup(r.PathValue("id"))
	if !ok || !allowed(r, task.ProjectID) || !ticketAllowed(r, s.Runs.Get(task.RunID).TicketID) {
		writeError(w, ErrNotFound)
		return
	}
	var review SolutionReview
	if err := decodeJSON(w, r, &review); err != nil {
		writeError(w, err)
		return
	}
	sol, exists := taskSolution(s, task)
	if exists && !ticketAllowed(r, sol.SourceTicketID) {
		writeError(w, ErrNotFound)
		return
	}
	result, err := api.Engine.ReviewSolution(task.ID, review)
	if err != nil {
		writeError(w, err)
		return
	}
	result.StoredFields = nil
	writeJSON(w, 200, result)
}
func (api *API) snapshot(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	// Snapshots that nothing in memory references may live only in storage.
	snap, ok, err := api.Engine.Store.LookupSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Snapshot storage is unavailable."})
		return
	}
	current := s.Tickets.Get(snap.Ticket.ID)
	if !ok || snap.Ticket.Private || current.Private || !allowed(r, snap.Ticket.ProjectID) || !ticketAllowed(r, snap.Ticket.ID) || snap.AvailableAt.After(api.Engine.Now()) {
		writeError(w, ErrNotFound)
		return
	}
	writeJSON(w, 200, snap)
}
func (api *API) advanceClock(w http.ResponseWriter, r *http.Request) {
	cfg := api.Engine.Config.Replay
	if !cfg.Enabled {
		writeError(w, ErrNotFound)
		return
	}
	for _, project := range cfg.Projects {
		if !allowed(r, project) {
			writeJSON(w, 403, map[string]string{"error": "Replay clock operator scope required."})
			return
		}
	}
	var req struct {
		Time time.Time `json:"time"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	err := api.Engine.update(func(s *State) error {
		old := s.ReplayClock
		if old.IsZero() {
			old = api.Engine.Now()
		}
		if req.Time.IsZero() || req.Time.Before(old) {
			return fmt.Errorf("%w: replay clock must advance monotonically", ErrInvalid)
		}
		s.ReplayClock = req.Time
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	for {
		old := api.Engine.clock.Load()
		if old >= req.Time.UnixNano() || api.Engine.clock.CompareAndSwap(old, req.Time.UnixNano()) {
			break
		}
	}
	writeJSON(w, 200, map[string]any{"time": api.Engine.Now()})
}
