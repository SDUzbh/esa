package esa

import "time"

func (e *Engine) withdrawSourceSolutions(s *State, ticketID, reason string) {
	for id, task := range s.Investigations {
		old, ok := s.Solutions[task.SolutionID]
		if !ok || old.SourceTicketID != ticketID || old.State != "trusted" {
			continue
		}
		next := old
		next.ID = randomID("solution_")
		next.Supersedes = old.ID
		next.Revision++
		next.State = "withdrawn"
		next.Note = reason
		next.AvailableAt = e.Now()
		next.RecordedAt = time.Now().UTC()
		s.Solutions[next.ID] = next
		task.SolutionID = next.ID
		task.State = "withdrawn"
		task.UpdatedAt = e.Now()
		s.Investigations[id] = task
		run := s.Runs[task.RunID]
		run.State = "invalidated"
		run.UpdatedAt = e.Now()
		s.Runs[run.ID] = run
	}
}
