package esa

import "sort"

// Supply only public, time-visible families with a matching environment scope.
// This is candidate retrieval; the Agent's choice remains tentative.
func issueCandidates(s State, task Investigation, ticket Ticket) []IssueBrief {
	rows := []IssueBrief{{ID: task.IssueID, Title: s.Issues[task.IssueID].Title}}
	if s.IssueOverrides[ticket.ID] != "" {
		return rows
	}
	ids := []string{}
	for id, issue := range s.Issues {
		if id != task.IssueID && issue.ProjectID == task.ProjectID && !issue.CreatedAt.After(task.Contract.ObservationCutoff) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		keys := []string{}
		for _, group := range s.Groups {
			if group.IssueID != id || group.ScopeHash != investigationScope(ticket) || group.CreatedAt.After(task.Contract.ObservationCutoff) {
				continue
			}
			other := s.Investigations[group.CurrentTaskID]
			source := s.Runs[other.RunID].Input
			current, ok := s.Tickets[source.ID]
			if !ok || current.Private || current.ProjectID != ticket.ProjectID || source.ForwardedAt.After(task.Contract.ObservationCutoff) {
				continue
			}
			if len(intersects(keywords(ticketText(ticket)), keywords(ticketText(source)))) < 2 && len(intersects(signals(ticket), signals(source))) == 0 {
				continue
			}
			keys = append(keys, source.SourceKey)
		}
		if len(keys) > 0 {
			rows = append(rows, IssueBrief{ID: id, Title: s.Issues[id].Title, TicketKeys: unique(keys)})
		}
		if len(rows) == 10 {
			break
		}
	}
	return rows
}

func applyIssueAssignment(s *State, task *Investigation, report Report, candidates []IssueBrief) {
	if report.Issue == nil {
		return
	}
	id := report.Issue.ExistingID
	if id == "" || id == task.IssueID {
		return
	}
	permitted := false
	for _, candidate := range candidates {
		permitted = permitted || candidate.ID == id
	}
	if !permitted {
		return
	}
	currentCandidates := issueCandidates(*s, *task, s.Runs[task.RunID].Input)
	permitted = false
	for _, candidate := range currentCandidates {
		permitted = permitted || candidate.ID == id
	}
	if !permitted {
		return
	}
	for _, link := range s.Memberships {
		if link.InvestigationID == task.ID && s.IssueOverrides[link.TicketID] != "" {
			return // A model cannot undo an engineer's explicit split/assignment.
		}
	}
	task.IssueID = id
	group := s.Groups[task.GroupID]
	group.IssueID = id
	s.Groups[group.ID] = group
	for key, link := range s.Memberships {
		if link.InvestigationID == task.ID && link.State == "tentative" {
			link.IssueID = id
			link.Version++
			link.Basis = "Agent proposed family: " + report.Issue.Reason
			s.Memberships[key] = link
			s.MembershipHistory = append(s.MembershipHistory, link)
		}
	}
}
