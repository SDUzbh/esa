package esa

import (
	"fmt"
	"strings"
)

func boundedIssueText(value string) string {
	text := []rune(strings.TrimSpace(value))
	if len(text) > 1000 {
		text = text[:1000]
	}
	return string(text)
}

func (e *Engine) applyIssueAssignment(s *State, task *Investigation, report Report, candidates []IssueRecord) error {
	if report.Issue == nil {
		return fmt.Errorf("%w: Agent issue proposal is required", ErrInvalid)
	}
	id, reason, title := report.Issue.ExistingID, report.Issue.Reason, report.Issue.Title
	var reused *IssueRecord
	if report.Routing != nil && report.Routing.Action == "reuse" {
		for _, record := range candidates {
			if recordPath(record) == report.Routing.RecordPath {
				copy := record
				reused = &copy
				break
			}
		}
		if reused == nil {
			return fmt.Errorf("%w: reuse record is outside the frozen directory", ErrInvalid)
		}
		if reused.CaseID != "" && s.Cases[reused.CaseID].Version != reused.CaseVersion {
			return fmt.Errorf("%w: selected case revision changed", ErrConflict)
		}
		if reused.SolutionID != "" {
			selected := s.Solutions[reused.SolutionID]
			if s.Investigations[selected.InvestigationID].SolutionID != reused.SolutionID {
				return fmt.Errorf("%w: selected solution revision changed", ErrConflict)
			}
		}
		stillAvailable := false
		for _, record := range issueRecords(*s, *task, s.Runs[task.RunID].Input) {
			if recordPath(record) == recordPath(*reused) && hash(record) == hash(*reused) {
				stillAvailable = true
			}
		}
		if !stillAvailable {
			return fmt.Errorf("%w: reuse source changed or was withdrawn", ErrConflict)
		}
	}
	if override := s.IssueOverrides[s.Runs[task.RunID].TicketID]; override != "" {
		if id != "" && id != override {
			return fmt.Errorf("%w: Agent cannot override reviewed issue membership", ErrConflict)
		}
		id = override
	}
	if id != "" {
		permitted := false
		for _, candidate := range candidates {
			permitted = permitted || candidate.IssueID == id
		}
		current := false
		for _, candidate := range issueRecords(*s, *task, s.Runs[task.RunID].Input) {
			current = current || candidate.IssueID == id
		}
		if !permitted || !current {
			return fmt.Errorf("%w: issue is no longer eligible", ErrConflict)
		}
	} else {
		id = randomID("issue_")
		if reused != nil && reused.CaseID != "" {
			id = "issue_" + hash(reused.CaseID)[:32]
		}
		if _, exists := s.Issues[id]; !exists {
			s.Issues[id] = EngineeringIssue{ID: id, ProjectID: task.ProjectID, Title: boundedIssueText(title), CreatedAt: e.Now()}
		}
	}
	if reused != nil && reused.CaseID != "" && reused.IssueID == "" && s.Cases[reused.CaseID].IssueID != id {
		c := s.Cases[reused.CaseID]
		c.IssueID = id
		c.Version++
		c.UpdatedAt = e.Now()
		s.Cases[c.ID] = c
		s.CaseHistory = append(s.CaseHistory, c)
	}
	task.IssueID = id
	group := s.Groups[task.GroupID]
	group.IssueID = id
	s.Groups[group.ID] = group
	for key, link := range s.Memberships {
		if link.InvestigationID == task.ID && link.State == "tentative" {
			link.IssueID = id
			link.Version++
			link.Basis = "Agent proposed family: " + reason
			link.AvailableAt = e.Now()
			s.Memberships[key] = link
			s.MembershipHistory = append(s.MembershipHistory, link)
		}
	}
	return nil
}
