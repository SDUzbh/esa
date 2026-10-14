package esa

import "time"

func caseScope(c Case) string {
	return investigationScope(Ticket{ProjectID: c.ProjectID, Components: c.Components, Versions: c.Versions, Environment: c.Environment})
}
func canonicalSolution(s State, issueID, scope string) (SolutionRevision, bool) {
	var selected SolutionRevision
	for _, task := range s.Investigations {
		sol, ok := s.Solutions[task.SolutionID]
		if !ok || task.IssueID != issueID || sol.ScopeHash != scope || s.Groups[task.GroupID].CurrentTaskID != task.ID {
			continue
		}
		if selected.ID == "" || sol.AvailableAt.After(selected.AvailableAt) || sol.AvailableAt.Equal(selected.AvailableAt) && (sol.Revision > selected.Revision || sol.Revision == selected.Revision && sol.ID < selected.ID) {
			selected = sol
		}
	}
	return selected, selected.ID != ""
}
func (e *Engine) syncCases(s *State, sol SolutionRevision) {
	for id, c := range s.Cases {
		if c.IssueID != sol.IssueID || caseScope(c) != sol.ScopeHash || c.State == "invalidated" || c.State == "duplicate" || c.State == "archived" {
			continue
		}
		if c.SolutionID == "" && hash(c.Resolution) != hash(sol.Resolution) {
			continue
		}
		next := c
		next.SolutionID = sol.ID
		next.Resolution = sol.Resolution
		next.State = sol.State
		if sol.State == "proposed" {
			next.State = "draft"
		}
		next.ReviewedBy, next.ReviewNote = sol.Reviewer, sol.Note
		if sol.State == "trusted" {
			next.TrustedAt = sol.AvailableAt
		} else {
			next.TrustedAt = time.Time{}
		}
		if hash(next) == hash(c) {
			continue
		}
		next.Version++
		next.UpdatedAt = e.Now()
		s.Cases[id] = next
		s.CaseHistory = append(s.CaseHistory, next)
	}
}

// Publish one canonical revision, then update references for the same Issue and
// scope. Historical revisions and original diagnosis reports remain immutable.
func (e *Engine) publishSolution(s *State, task Investigation, old, next SolutionRevision) SolutionRevision {
	next.StoredFields = nil
	next.ID, next.Supersedes = randomID("solution_"), old.ID
	next.Revision = old.Revision + 1
	next.InvestigationID = old.InvestigationID
	next.RunID = old.RunID
	next.AvailableAt, next.RecordedAt = e.Now(), time.Now().UTC()
	s.Solutions[next.ID] = next
	for id, current := range s.Investigations {
		if current.SolutionID == "" || current.IssueID != task.IssueID || s.Groups[current.GroupID].ScopeHash != next.ScopeHash || s.Groups[current.GroupID].CurrentTaskID != current.ID {
			continue
		}
		current.SolutionID, current.State, current.UpdatedAt = next.ID, next.State, e.Now()
		s.Investigations[id] = current
	}
	e.syncCases(s, next)
	e.invalidateApplications(s)
	return next
}

func sameResolution(a, b Resolution) bool {
	return a.Cause == b.Cause && a.Solution == b.Solution && a.Validation == b.Validation
}
