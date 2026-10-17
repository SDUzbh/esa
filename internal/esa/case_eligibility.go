package esa

// Historical ranking never bypasses current source privacy. A reviewed split
// also excludes repairs attributed to a different family, even when the words
// and affected versions remain identical.
func eligibleCaseHistory(s State, t Ticket) []Case {
	rows := []Case{}
	for _, c := range s.CaseHistory {
		source, ok := s.Tickets.Lookup(c.TicketID)
		if !ok || source.Private || source.ProjectID != t.ProjectID {
			continue
		}
		if override := s.IssueOverrides.Get(t.ID); override != "" && c.IssueID != override {
			continue
		}
		rows = append(rows, c)
	}
	return rows
}
