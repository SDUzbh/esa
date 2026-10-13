package esa

// Investigations own execution state; Run.State is its API/history projection.
func projectInvestigationStates(s *State) {
	for _, task := range s.Investigations {
		run, ok := s.Runs[task.RunID]
		if !ok {
			continue
		}
		run.State = task.State
		switch task.State {
		case "agent_proposed":
			run.State = "awaiting_review"
		case "trusted":
			run.State = "completed"
		case "withdrawn":
			run.State = "invalidated"
		}
		if s.Tickets[run.TicketID].Private {
			run.State = "restricted"
		}
		run.UpdatedAt = task.UpdatedAt
		s.Runs[run.ID] = run
	}
}

// A family can have multiple scopes. Never copy the last reviewed scope's state
// onto the family and accidentally mark unrelated investigations as repaired.
func issueState(s State, id string, visible map[string]bool) string {
	state := ""
	for _, group := range s.Groups {
		if group.IssueID != id {
			continue
		}
		allowed := false
		for _, link := range s.Memberships {
			allowed = allowed || link.InvestigationID == group.CurrentTaskID && link.State == "tentative" && visible[link.TicketID]
		}
		if !allowed {
			continue
		}
		task, ok := s.Investigations[group.CurrentTaskID]
		if !ok {
			continue
		}
		if state != "" && state != task.State {
			return "mixed"
		}
		state = task.State
	}
	if state == "" {
		return "tentative"
	}
	return state
}
