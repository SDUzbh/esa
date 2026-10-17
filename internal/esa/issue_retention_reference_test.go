package esa

import "time"

// The guard runs this full-scan Issue retention, the implementation before
// the by-Issue indexes, beside the indexed one and requires identical states.
func init() { issueRetentionReference = maintainIssuesByScan }

func maintainIssuesByScan(s *State, at time.Time) bool {
	live := map[string]bool{}
	for _, link := range s.Memberships.All() {
		t, ok := s.Tickets.Lookup(link.TicketID)
		if ok && !t.Private && link.State == "tentative" && link.IssueID != "" {
			live[link.IssueID] = true
		}
	}
	for _, c := range s.Cases.All() {
		t, ok := s.Tickets.Lookup(c.TicketID)
		if ok && !t.Private && t.Closed && c.State != "invalidated" && c.State != "duplicate" && c.State != "archived" {
			live[c.IssueID] = true
		}
	}
	for _, sol := range s.Solutions.All() {
		if live[sol.IssueID] || s.Investigations.Get(sol.InvestigationID).SolutionID != sol.ID || sol.State != "proposed" && sol.State != "trusted" {
			continue
		}
		t, ok := s.Tickets.Lookup(sol.SourceTicketID)
		run := s.Runs.Get(sol.RunID)
		if !ok || t.Private || t.Closed || run.TicketID != t.ID || investigationSignature(run.Input) != investigationSignature(t) {
			continue
		}
		var primary string
		var active IssueTicketLink
		for _, link := range s.Memberships.All() {
			if link.TicketID == t.ID && link.State == "tentative" && (active.ID == "" || link.AvailableAt.After(active.AvailableAt) || link.AvailableAt.Equal(active.AvailableAt) && link.ID < active.ID) {
				active, primary = link, link.IssueID
			}
		}
		if override := s.IssueOverrides.Get(t.ID); override != "" {
			primary = override
		}
		if primary == "" || primary == sol.IssueID {
			live[sol.IssueID] = true
		}
	}
	removed := false
	for _, id := range s.Issues.Keys() {
		if issue := s.Issues.Get(id); issue.AccessOrder > s.IssueAccessSequence {
			s.IssueAccessSequence = issue.AccessOrder
		}
		if !live[id] {
			removeIssueByScan(s, id, "empty", at)
			removed = true
		}
	}
	if s.Issues.Len() <= IssueLimit {
		return removed
	}
	for _, issue := range leastRecentIssues(*s) {
		removeIssueByScan(s, issue.ID, "least recently used", at)
	}
	return true
}

func removeIssueByScan(s *State, id, reason string, at time.Time) {
	s.Issues.Delete(id)
	for _, key := range s.Memberships.Keys() {
		if link := s.Memberships.Get(key); link.IssueID == id && link.State == "tentative" {
			detachForRemovedIssue(s, key, reason, at)
		}
	}
	for ticket, issue := range s.IssueOverrides.All() {
		if issue == id {
			s.IssueOverrides.Delete(ticket)
		}
	}
}
