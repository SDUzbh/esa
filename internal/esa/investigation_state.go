package esa

// Investigations own execution state; Run.State is its API/history projection.
// projectInvestigationStates projects every investigation onto its run.
func projectInvestigationStates(s *State) {
	for _, task := range s.Investigations.All() {
		projectRun(s, task)
	}
}

// projectChangedInvestigations projects only what this Update can have
// changed: investigations and runs it wrote, found by diffing the persistent
// tables in O(changes). A ticket whose privacy flipped can affect any of its
// runs, so it falls back to the full projection. In tests the guard checks
// the result against the full projection on every Update.
func projectChangedInvestigations(old, s *State) {
	for _, id := range diffTable(old.Tickets, s.Tickets).Upserted {
		if old.Tickets.Get(id).Private != s.Tickets.Get(id).Private {
			projectInvestigationStates(s)
			return
		}
	}
	tasks := map[string]bool{}
	for _, id := range diffTable(old.Investigations, s.Investigations).Upserted {
		tasks[id] = true
	}
	for _, id := range diffTable(old.Runs, s.Runs).Upserted {
		if task := s.Runs.Get(id).InvestigationID; task != "" {
			tasks[task] = true
		}
	}
	for id := range tasks {
		if task, ok := s.Investigations.Lookup(id); ok {
			projectRun(s, task)
		}
	}
}

// projectRun sets the run's State and UpdatedAt from its investigation, and
// writes the run only if that changes it.
func projectRun(s *State, task Investigation) {
	run, ok := s.Runs.Lookup(task.RunID)
	if !ok {
		return
	}
	state := task.State
	switch task.State {
	case "agent_proposed", "reuse_proposed":
		state = "awaiting_review"
	case "trusted", "applied":
		state = "completed"
	case "withdrawn":
		state = "invalidated"
	}
	if s.Tickets.Get(run.TicketID).Private {
		state = "restricted"
	}
	if run.State != state || run.UpdatedAt != task.UpdatedAt {
		run.State, run.UpdatedAt = state, task.UpdatedAt
		s.Runs.Put(run.ID, run)
	}
}

// A family can have multiple scopes. Never copy the last reviewed scope's state
// onto the family and accidentally mark unrelated investigations as repaired.
func issueState(s State, id string, visible map[string]bool) string {
	state := ""
	links := s.indexes.tentativeByIssue.lookup(s.Memberships, tentativeIssue, id)
	for _, key := range s.indexes.groupsByIssue.lookup(s.Groups, groupIssue, id) {
		group := s.Groups.Get(key)
		allowed := false
		for _, key := range links {
			link := s.Memberships.Get(key)
			allowed = allowed || link.InvestigationID == group.CurrentTaskID && visible[link.TicketID]
		}
		if !allowed {
			continue
		}
		task, ok := s.Investigations.Lookup(group.CurrentTaskID)
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
