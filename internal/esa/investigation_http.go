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
	if row.EngineeringIssue != nil {
		copy := *row.EngineeringIssue
		copy.Title = row.Ticket.Subject
		row.EngineeringIssue = &copy
	}
	if row.Solution != nil {
		sol := row.Solution
		source, ok := s.Tickets[sol.SourceTicketID]
		if !ok || source.Private || !ticketAllowed(r, source.ID) || !allowed(r, source.ProjectID) || sol.AvailableAt.After(at) {
			row.Solution = nil
		} else if sol.State == "rejected" || sol.State == "withdrawn" {
			copy := *sol
			report := Report{Summary: "The shared proposal was rejected or withdrawn. Earlier evidence remains in the audit history."}
			copy.Report = nil
			row.Solution = &copy
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
	t, ok := api.Engine.Store.Snapshot().Tickets[r.PathValue("id")]
	if !ok || t.Private || !allowed(r, t.ProjectID) || !ticketAllowed(r, t.ID) {
		writeError(w, ErrNotFound)
		return
	}
	var review MembershipReview
	if err := decodeJSON(w, r, &review); err != nil {
		writeError(w, err)
		return
	}
	receipt, err := api.Engine.ReviewMembership(t.ID, review)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}
func taskVisible(r *http.Request, s State, task Investigation) bool {
	if !allowed(r, task.ProjectID) {
		return false
	}
	if source, ok := s.Tickets[s.Runs[task.RunID].TicketID]; ok && !source.Private && ticketAllowed(r, source.ID) {
		return true
	}
	for _, link := range s.Memberships {
		t, ok := s.Tickets[link.TicketID]
		if ok && link.InvestigationID == task.ID && link.State == "tentative" && !t.Private && ticketAllowed(r, t.ID) {
			return true
		}
	}
	return false
}
func (api *API) solutionHistory(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	task, ok := s.Investigations[r.PathValue("id")]
	source := s.Tickets[s.Runs[task.RunID].TicketID]
	if !ok || source.Private || !allowed(r, task.ProjectID) || !ticketAllowed(r, source.ID) {
		writeError(w, ErrNotFound)
		return
	}
	rows := []SolutionRevision{}
	for _, solution := range s.Solutions {
		source, exists := s.Tickets[solution.SourceTicketID]
		if solution.InvestigationID == task.ID && !solution.AvailableAt.After(api.Engine.Now()) && exists && !source.Private && allowed(r, source.ProjectID) && ticketAllowed(r, source.ID) {
			rows = append(rows, solution)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Revision < rows[j].Revision })
	writeJSON(w, 200, map[string]any{"solutions": rows})
}
func (api *API) engineeringIssues(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	rows := []map[string]any{}
	for _, issue := range s.Issues {
		if !allowed(r, issue.ProjectID) {
			continue
		}
		ids := map[string]bool{}
		title := ""
		for _, link := range s.Memberships {
			t, ok := s.Tickets[link.TicketID]
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
		copy := issue
		copy.Title = s.Tickets[title].Subject
		copy.State = issueState(s, issue.ID, ids)
		rows = append(rows, map[string]any{"issue": copy, "ticket_ids": members, "visible_ticket_count": len(members)})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["issue"].(EngineeringIssue).ID < rows[j]["issue"].(EngineeringIssue).ID
	})
	writeJSON(w, 200, map[string]any{"issues": rows, "total_count": len(rows)})
}
func (api *API) investigation(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	task, ok := s.Investigations[r.PathValue("id")]
	if !ok || !taskVisible(r, s, task) {
		writeError(w, ErrNotFound)
		return
	}
	result := map[string]any{"investigation": task}
	source := s.Tickets[s.Runs[task.RunID].TicketID]
	if !source.Private && ticketAllowed(r, source.ID) {
		result["run"] = visibleRun(r, s, s.Runs[task.RunID])
		if sol, ok := s.Solutions[task.SolutionID]; ok && !sol.AvailableAt.After(api.Engine.Now()) {
			evidenceSource, exists := s.Tickets[sol.SourceTicketID]
			if exists && !evidenceSource.Private && allowed(r, evidenceSource.ProjectID) && ticketAllowed(r, evidenceSource.ID) {
				result["solution"] = sol
			}
		}
	}
	writeJSON(w, 200, result)
}
func (api *API) reviewSolution(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	task, ok := s.Investigations[r.PathValue("id")]
	if !ok || !allowed(r, task.ProjectID) || !ticketAllowed(r, s.Runs[task.RunID].TicketID) {
		writeError(w, ErrNotFound)
		return
	}
	var review SolutionReview
	if err := decodeJSON(w, r, &review); err != nil {
		writeError(w, err)
		return
	}
	result, err := api.Engine.ReviewSolution(task.ID, review)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (api *API) snapshot(w http.ResponseWriter, r *http.Request) {
	s := api.Engine.Store.Snapshot()
	snap, ok := s.Snapshots[r.PathValue("id")]
	current := s.Tickets[snap.Ticket.ID]
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
	err := api.Engine.Store.Update(func(s *State) error {
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
