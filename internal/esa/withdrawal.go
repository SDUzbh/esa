package esa

func (e *Engine) withdrawSourceSolutions(s *State, ticketID, reason string) {
	for _, task := range s.Investigations.All() {
		old, ok := s.Solutions.Lookup(task.SolutionID)
		if !ok || old.SourceTicketID != ticketID || old.State != SolutionTrusted {
			continue
		}
		next := old
		next.State = SolutionWithdrawn
		next.Note = reason
		e.publishSolution(s, task, old, next)

	}
}
