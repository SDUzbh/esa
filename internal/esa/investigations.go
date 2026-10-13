package esa

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (e *Engine) workflowVersion() string {
	if e.Config.V2.Enabled {
		return "5"
	}
	return WorkflowVersion
}
func (e *Engine) Version() string { return e.workflowVersion() }
func prepareV2(s *State) {
	if s.Snapshots == nil {
		s.Snapshots = map[string]TicketSnapshot{}
	}
	if s.LatestSnapshot == nil {
		s.LatestSnapshot = map[string]string{}
	}
	if s.Issues == nil {
		s.Issues = map[string]EngineeringIssue{}
	}
	if s.Memberships == nil {
		s.Memberships = map[string]IssueTicketLink{}
	}
	if s.IssueOverrides == nil {
		s.IssueOverrides = map[string]string{}
	}
	if s.Investigations == nil {
		s.Investigations = map[string]Investigation{}
	}
	if s.Groups == nil {
		s.Groups = map[string]InvestigationGroup{}
	}
	if s.Solutions == nil {
		s.Solutions = map[string]SolutionRevision{}
	}
}
func captureSnapshot(s *State, event Event) {
	prepareV2(s)
	id := "snapshot_" + hash(event.ID)
	t := event.Ticket
	s.Snapshots[id] = TicketSnapshot{ID: id, Hash: hash(t), EventID: event.ID, Ticket: t, EffectiveAt: t.UpdatedAt, AvailableAt: event.ForwardedAt, RecordedAt: time.Now().UTC(), Cutoff: event.ForwardedAt}
	s.LatestSnapshot[t.ID] = id
}

// Conservative operational deduplication: exact diagnostic content and exact
// context, not a generic exception or a similarity score. This does not prove
// common causality. Critical parameter values and public comment text remain.
func investigationScope(t Ticket) string {
	return hash(struct {
		Project              string
		Components, Versions []string
		Environment          map[string]string
	}{t.ProjectID, unique(t.Components), unique(t.Versions), t.Environment})
}
func investigationSignature(t Ticket) string {
	return hash(struct{ Scope, Input string }{investigationScope(t), diagnosticHash(t)})
}
func (e *Engine) detachChangedMemberships(s *State, t Ticket) {
	for id, link := range s.Memberships {
		if link.TicketID != t.ID || link.State != "tentative" {
			continue
		}
		old := s.Snapshots[link.SnapshotID].Ticket
		if t.Private || t.Closed || investigationSignature(old) != investigationSignature(t) {
			link.State = "detached"
			link.Version++
			s.Memberships[id] = link
			s.MembershipHistory = append(s.MembershipHistory, link)
		}
	}
	if t.Private {
		for id, task := range s.Investigations {
			run := s.Runs[task.RunID]
			if run.TicketID == t.ID && task.State != "withdrawn" {
				task.State = "withdrawn"
				task.UpdatedAt = e.Now()
				s.Investigations[id] = task
				if solution, ok := s.Solutions[task.SolutionID]; ok && solution.State != "withdrawn" {
					next := solution
					next.ID = randomID("solution_")
					next.Revision++
					next.Supersedes = solution.ID
					next.State = "withdrawn"
					next.AvailableAt = e.Now()
					next.RecordedAt = time.Now().UTC()
					s.Solutions[next.ID] = next
					task.SolutionID = next.ID
					s.Investigations[id] = task
				}
			}
		}
	}
}
func taskHasActiveMembers(s State, task Investigation) bool {
	for _, link := range s.Memberships {
		if link.InvestigationID != task.ID || link.State != "tentative" {
			continue
		}
		t, ok := s.Tickets[link.TicketID]
		if ok && !t.Private && !t.Closed && s.Latest[link.TicketID] == task.RunID && investigationSignature(t) == s.Groups[task.GroupID].Signature {
			return true
		}
	}
	return false
}
func availableTask(s State, task Investigation, cutoff time.Time) bool {
	if task.CreatedAt.After(cutoff) || task.Contract.ObservationCutoff.After(cutoff) {
		return false
	}
	run := s.Runs[task.RunID]
	source, ok := s.Tickets[run.TicketID]
	if !ok || source.Private || run.WorkflowVersion != "5" {
		return false
	}
	switch task.State {
	case "queued", "running", "awaiting_configuration", "failed", "awaiting_information", "rejected":
		return true
	case "agent_proposed", "trusted":
		solution, ok := s.Solutions[task.SolutionID]
		return ok && (solution.State == "proposed" || solution.State == "trusted")
	}
	return false
}
func (e *Engine) enqueueShared(s *State, t Ticket, now time.Time, force bool) Receipt {
	prepareV2(s)
	snap, exists := s.Snapshots[s.LatestSnapshot[t.ID]]
	cutoff := snap.Cutoff
	if !exists {
		cutoff = now
	}
	matches, route, status, missing := e.Decide(t, eligibleCaseHistory(*s, t), cutoff)
	if !exists {
		route, status, missing = "manual_forward", "awaiting_forward", []string{"Forward a new immutable snapshot before V2 diagnosis."}
	}
	if !force {
		if old, ok := s.Runs[s.Latest[t.ID]]; ok && old.WorkflowVersion == "5" && old.InputHash == diagnosticHash(t) && old.CaseGeneration == s.Generation && old.RuleVersion == e.Config.Version {
			if old.InvestigationID == "" || availableTask(*s, s.Investigations[old.InvestigationID], cutoff) {
				return Receipt{TicketID: t.ID, RunID: old.ID, Action: "cached"}
			}
		}
	}
	if route != "engineering_analysis" {
		r := Run{ID: randomID("run_"), TicketID: t.ID, ProjectID: t.ProjectID, WorkflowVersion: "5", RuleVersion: e.Config.Version, Input: t, InputHash: diagnosticHash(t), SnapshotID: snap.ID, ObservationCutoff: cutoff, CaseGeneration: s.Generation, Route: route, State: status, Matches: matches, MissingInformation: missing, CreatedAt: now, UpdatedAt: now}
		if route == "case_reuse" {
			r.Report.Summary = "Applicable trusted cases found; verify the recommendation in this environment."
			for _, m := range matches {
				if m.Applicable {
					r.Report.Recommendations = append(r.Report.Recommendations, m.Solution)
					r.Report.Evidence = append(r.Report.Evidence, Evidence{Source: "resolved_case", Reference: fmt.Sprintf("%s@%d", m.CaseID, m.CaseVersion), Quote: m.Solution})
				}
			}
		}
		s.Runs[r.ID] = r
		s.Latest[t.ID] = r.ID
		return Receipt{TicketID: t.ID, RunID: r.ID, Action: "created"}
	}
	signature := investigationSignature(t)
	groupID := "group_" + signature
	if override := s.IssueOverrides[t.ID]; override != "" {
		groupID = "group_" + hash(struct{ Signature, Issue string }{signature, override})
	}
	group, ok := s.Groups[groupID]
	if !ok {
		issue := EngineeringIssue{ID: randomID("issue_"), ProjectID: t.ProjectID, Title: t.Subject, State: "tentative", CreatedAt: now}
		if override, ok := s.Issues[s.IssueOverrides[t.ID]]; ok && override.ProjectID == t.ProjectID {
			issue = override
		}
		s.Issues[issue.ID] = issue
		group = InvestigationGroup{ID: groupID, IssueID: issue.ID, ProjectID: t.ProjectID, Signature: signature, ScopeHash: investigationScope(t), CreatedAt: cutoff}
	}
	if !force && availableTask(*s, s.Investigations[group.CurrentTaskID], cutoff) {
		task := s.Investigations[group.CurrentTaskID]
		link := IssueTicketLink{ID: randomID("link_"), IssueID: group.IssueID, TicketID: t.ID, SnapshotID: snap.ID, InvestigationID: task.ID, State: "tentative", Basis: "exact diagnostic and context signature; common causality unconfirmed", AvailableAt: cutoff}
		s.Memberships[link.ID] = link
		link.Version = 1
		s.Memberships[link.ID] = link
		s.MembershipHistory = append(s.MembershipHistory, link)
		s.Latest[t.ID] = task.RunID
		return Receipt{TicketID: t.ID, RunID: task.RunID, Action: "linked_investigation"}
	}
	id := randomID("investigation_")
	runID := randomID("run_")
	contract := TaskContract{Objective: "Locate the reported defect and provide source-pinned evidence, a repair recommendation and verification steps.", SnapshotID: snap.ID, InputHash: snap.Hash, ObservationCutoff: cutoff, IssueID: group.IssueID, ReadOnly: true, CompletionCriteria: []string{"Address the reported behavior using supplied historical evidence", "Separate supported findings from uncertainty", "Do not modify product source or close customer tickets"}}
	task := Investigation{ID: id, GroupID: groupID, IssueID: group.IssueID, ProjectID: t.ProjectID, RunID: runID, State: "queued", Contract: contract, CreatedAt: cutoff, UpdatedAt: now}
	run := Run{ID: runID, InvestigationID: id, IssueID: group.IssueID, SnapshotID: snap.ID, ObservationCutoff: cutoff, TicketID: t.ID, ProjectID: t.ProjectID, WorkflowVersion: "5", RuleVersion: e.Config.Version, Input: t, InputHash: diagnosticHash(t), CaseGeneration: s.Generation, Route: "engineering_analysis", State: "queued", Matches: matches, CreatedAt: now, UpdatedAt: now}
	group.CurrentTaskID = id
	s.Groups[groupID] = group
	s.Investigations[id] = task
	s.Runs[runID] = run
	s.Latest[t.ID] = runID
	link := IssueTicketLink{ID: randomID("link_"), IssueID: group.IssueID, TicketID: t.ID, SnapshotID: snap.ID, InvestigationID: id, State: "tentative", Basis: "new exact-signature investigation", AvailableAt: cutoff}
	s.Memberships[link.ID] = link
	link.Version = 1
	s.Memberships[link.ID] = link
	s.MembershipHistory = append(s.MembershipHistory, link)
	return Receipt{TicketID: t.ID, RunID: runID, Action: "created"}
}

func (e *Engine) ReviewSolution(id string, review SolutionReview) (SolutionRevision, error) {
	var result SolutionRevision
	err := e.Store.Update(func(s *State) error {
		task, ok := s.Investigations[id]
		if !ok {
			return ErrNotFound
		}
		old, ok := s.Solutions[task.SolutionID]
		if !ok {
			return ErrNotFound
		}
		if review.ExpectedSolutionID != old.ID {
			return fmt.Errorf("%w: solution revision changed", ErrConflict)
		}
		if strings.TrimSpace(review.Reviewer) == "" || !contains([]string{"approve", "reject", "withdraw"}, review.Action) {
			return fmt.Errorf("%w: reviewer and valid action required", ErrInvalid)
		}
		if review.Action == "approve" && (strings.TrimSpace(review.Resolution.Cause) == "" || strings.TrimSpace(review.Resolution.Solution) == "" || strings.TrimSpace(review.Resolution.Validation) == "") {
			return fmt.Errorf("%w: cause, solution and verification are required", ErrInvalid)
		}
		source := s.Tickets[s.Runs[task.RunID].TicketID]
		if source.Private {
			return ErrConflict
		}
		result = old
		result.ID = randomID("solution_")
		result.Revision++
		result.Supersedes = old.ID
		result.Origin = "engineer"
		result.Reviewer = review.Reviewer
		result.Note = review.Note
		result.AvailableAt = e.Now()
		result.RecordedAt = time.Now().UTC()
		switch review.Action {
		case "approve":
			result.State = "trusted"
			result.Resolution = review.Resolution
		case "reject":
			result.State = "rejected"
		case "withdraw":
			result.State = "withdrawn"
		}
		s.Solutions[result.ID] = result
		task.SolutionID = result.ID
		task.State = result.State
		task.UpdatedAt = e.Now()
		s.Investigations[id] = task
		issue := s.Issues[task.IssueID]
		issue.State = result.State
		s.Issues[issue.ID] = issue
		run := s.Runs[task.RunID]
		switch result.State {
		case "trusted":
			run.State = "completed"
		case "rejected":
			run.State = "rejected"
		case "withdrawn":
			run.State = "invalidated"
		}
		run.UpdatedAt = e.Now()
		s.Runs[run.ID] = run
		return nil
	})
	return result, err
}

func sortedInvestigations(s State) []Investigation {
	rows := make([]Investigation, 0, len(s.Investigations))
	for _, task := range s.Investigations {
		rows = append(rows, task)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	return rows
}

func fairInvestigations(s State) []Investigation {
	rows := sortedInvestigations(s)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].ProjectID, rows[j].ProjectID
		if a == b {
			return false
		}
		aAfter, bAfter := a > s.LastClaimedProject, b > s.LastClaimedProject
		if aAfter != bAfter {
			return aAfter
		}
		return a < b
	})
	return rows
}
