package esa

import "fmt"

func applicationSource(s State, app SolutionApplication) (SolutionRevision, bool) {
	if app.SolutionID != "" {
		sol, ok := s.Solutions[app.SolutionID]
		if !ok {
			return sol, false
		}
		if _, exists := s.Issues[sol.IssueID]; !exists {
			return sol, false
		}
		owner := s.Investigations[sol.InvestigationID]
		return sol, owner.SolutionID == sol.ID && (sol.State == "trusted" || sol.State == "proposed") && !s.Tickets[sol.SourceTicketID].Private
	}
	c, ok := s.Cases[app.CaseID]
	if !ok || c.Version != app.CaseVersion || c.State != "trusted" || s.Tickets[c.TicketID].Private {
		return SolutionRevision{}, false
	}
	if c.IssueID != "" {
		if _, exists := s.Issues[c.IssueID]; !exists {
			return SolutionRevision{}, false
		}
	}
	return SolutionRevision{ID: c.ID, IssueID: c.IssueID, ProjectID: c.ProjectID, SourceTicketID: c.TicketID, Origin: "engineer", State: c.State, Resolution: c.Resolution, AvailableAt: c.UpdatedAt}, true
}
func taskSolution(s State, task Investigation) (SolutionRevision, bool) {
	if task.Application != nil {
		return applicationSource(s, *task.Application)
	}
	sol, ok := s.Solutions[task.SolutionID]
	return sol, ok
}
func (e *Engine) reviewApplication(s *State, task Investigation, review SolutionReview) (SolutionRevision, error) {
	app := *task.Application
	source, valid := applicationSource(*s, app)
	if s.Groups[task.GroupID].CurrentTaskID != task.ID || s.Tickets[s.Runs[task.RunID].TicketID].Private {
		return source, fmt.Errorf("%w: application is no longer current", ErrConflict)
	}
	expected := app.SolutionID
	if expected == "" {
		expected = app.CaseID
	}
	if review.ExpectedSolutionID != expected || review.ExpectedApplicationRevision != app.Revision {
		return source, fmt.Errorf("%w: application changed", ErrConflict)
	}
	if review.Action == "approve" && !valid {
		return source, fmt.Errorf("%w: source solution is no longer available", ErrConflict)
	}
	state := map[string]string{"approve": "accepted", "reject": "rejected", "withdraw": "withdrawn"}[review.Action]
	if app.State == state {
		return source, nil
	}
	app.State = state
	app.Revision++
	app.Reviews = append(app.Reviews, ApplicationReview{State: state, Reviewer: review.Reviewer, Note: review.Note, AvailableAt: e.Now()})
	task.Application = &app
	task.UpdatedAt = e.Now()
	task.State = map[string]string{"accepted": "applied", "rejected": "rejected", "withdrawn": "withdrawn"}[state]
	s.Investigations[task.ID] = task
	return source, nil
}
func (e *Engine) invalidateApplications(s *State) {
	for id, task := range s.Investigations {
		if task.Application == nil || task.Application.State == "withdrawn" || task.Application.State == "rejected" {
			continue
		}
		if _, ok := applicationSource(*s, *task.Application); ok {
			continue
		}
		app := *task.Application
		app.State = "withdrawn"
		app.Revision++
		app.Reviews = append(app.Reviews, ApplicationReview{State: "withdrawn", Reviewer: "system", Note: "The referenced source changed or was withdrawn", AvailableAt: e.Now()})
		task.Application = &app
		task.State = "withdrawn"
		task.UpdatedAt = e.Now()
		s.Investigations[id] = task
	}
}

func taskSourceTicket(s State, task Investigation) string {
	if task.Application != nil {
		return task.Application.SourceTicketID
	}
	return s.Solutions[task.SolutionID].SourceTicketID
}
