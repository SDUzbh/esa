package esa

import "time"

// A reviewed case only supersedes the current shared solution associated with
// its own source ticket and exact applicability scope. Other releases retain
// their separate conclusions; the original Agent report is never rewritten.
func (e *Engine) publishReviewedCase(s *State, c Case) {
	for id, task := range s.Investigations {
		group := s.Groups[task.GroupID]
		run := s.Runs[task.RunID]
		if group.CurrentTaskID != id || run.TicketID != c.TicketID || investigationScope(s.Tickets[c.TicketID]) != group.ScopeHash {
			continue
		}
		old, ok := s.Solutions[task.SolutionID]
		if !ok {
			continue
		}
		next := old
		next.ID, next.Supersedes = randomID("solution_"), old.ID
		next.Revision++
		next.Origin, next.State = "engineer", c.State
		next.Resolution, next.Reviewer, next.Note = c.Resolution, c.ReviewedBy, c.ReviewNote
		next.AvailableAt, next.RecordedAt = e.Now(), time.Now().UTC()
		s.Solutions[next.ID] = next
		task.SolutionID, task.State, task.UpdatedAt = next.ID, next.State, e.Now()
		s.Investigations[id] = task
		if c.State == "trusted" {
			run.State = "completed"
		} else {
			run.State = "rejected"
		}
		run.UpdatedAt = e.Now()
		s.Runs[run.ID] = run
	}
}
