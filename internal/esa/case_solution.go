package esa

import "time"

// A reviewed case only supersedes the current shared solution associated with
// its own source ticket and exact applicability scope. Other releases retain
// their separate conclusions; the original Agent report is never rewritten.
func (e *Engine) publishReviewedCase(s *State, c Case) {
	for id, task := range s.Investigations {
		group := s.Groups[task.GroupID]
		if c.IssueID == "" || task.IssueID != c.IssueID || group.CurrentTaskID != id || investigationScope(s.Tickets[c.TicketID]) != group.ScopeHash {
			continue
		}
		old, ok := s.Solutions[task.SolutionID]
		if !ok {
			continue
		}
		next := old
		next.Origin, next.State = "engineer", c.State
		next.SourceTicketID = c.TicketID
		next.Resolution, next.Reviewer, next.Note = c.Resolution, c.ReviewedBy, c.ReviewNote
		e.publishSolution(s, task, old, next)
	}
}

// All review, case publication and withdrawal paths append through one authority.
func (e *Engine) publishSolution(s *State, task Investigation, old, next SolutionRevision) SolutionRevision {
	next.ID, next.Supersedes = randomID("solution_"), old.ID
	next.Revision = old.Revision + 1
	next.RunID, next.Report = task.RunID, nil
	next.AvailableAt, next.RecordedAt = e.Now(), time.Now().UTC()
	s.Solutions[next.ID] = next
	task.SolutionID, task.State, task.UpdatedAt = next.ID, next.State, e.Now()
	s.Investigations[task.ID] = task
	return next
}
