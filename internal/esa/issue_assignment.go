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
	var selection *RoutingResult
	if report.Routing != nil && report.Routing.Action == "reuse" {
		selection = report.Routing
	}
	if report.AssignmentRouting != nil && report.AssignmentRouting.Action == "assign" && report.AssignmentRouting.RecordPath != "" {
		selection = report.AssignmentRouting
	}
	if selection != nil {
		for _, record := range candidates {
			if recordPath(record) == selection.RecordPath && issueRecordHash(record) == selection.RecordHash {
				copy := record
				reused = &copy
				break
			}
		}
		if reused == nil {
			return fmt.Errorf("%w: selected record is outside the frozen directory", ErrInvalid)
		}
		if report.AssignmentRouting != nil && id != reused.IssueID {
			return fmt.Errorf("%w: grouping does not match the selected record", ErrInvalid)
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
	if report.AssignmentRouting != nil && report.AssignmentRouting.Action == "assign" && selection == nil && (id == "" || id != s.IssueOverrides[s.Runs[task.RunID].TicketID] || report.AssignmentRouting.RecordHash != "") {
		return fmt.Errorf("%w: grouping requires an eligible record or reviewed membership", ErrInvalid)
	}
	if override := s.IssueOverrides[s.Runs[task.RunID].TicketID]; override != "" {
		if report.AssignmentRouting != nil && report.AssignmentRouting.Action == "create" {
			return fmt.Errorf("%w: Routing Agent cannot create over reviewed membership", ErrConflict)
		}
		if id != "" && id != override {
			return fmt.Errorf("%w: Agent cannot override reviewed issue membership", ErrConflict)
		}
		id = override
	}
	if id != "" {
		if id != s.IssueOverrides[s.Runs[task.RunID].TicketID] {
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
		}
		issue, ok := s.Issues[id]
		if !ok || issue.ProjectID != task.ProjectID || issue.CreatedAt.After(task.Contract.ObservationCutoff) {
			return fmt.Errorf("%w: issue outside task scope", ErrConflict)
		}
	} else {
		id = randomID("issue_")
		if reused != nil && reused.CaseID != "" {
			id = "issue_" + hash(reused.CaseID)[:32]
		}
		titleSource := s.Runs[task.RunID].TicketID
		if reused != nil {
			titleSource = reused.SourceTicketID
		}
		if _, exists := s.Issues[id]; !exists {
			s.Issues[id] = EngineeringIssue{ID: id, ProjectID: task.ProjectID, Title: boundedIssueText(title), TitleSourceTicketID: titleSource, CreatedAt: e.Now()}
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
	touchIssue(s, id, e.Now())
	for key, group := range s.Groups {
		if group.CurrentTaskID == task.ID {
			group.IssueID = id
			s.Groups[key] = group
		}
	}
	for key, link := range s.Memberships {
		if link.InvestigationID == task.ID && link.State == "tentative" {
			if override := s.IssueOverrides[link.TicketID]; override != "" && override != id {
				continue
			}
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
