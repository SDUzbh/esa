package esa

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (e *Engine) Version() string { return WorkflowVersion }
func prepareInvestigations(s *State) {
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
	prepareInvestigations(s)
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
			if (run.TicketID == t.ID || taskSourceTicket(*s, task) == t.ID) && task.State != "withdrawn" {
				task.State = "withdrawn"
				task.UpdatedAt = e.Now()
				s.Investigations[id] = task
				if solution, ok := s.Solutions[task.SolutionID]; ok && solution.State != "withdrawn" {
					next := solution
					next.State = "withdrawn"
					next.Note = "The source ticket became private"
					e.publishSolution(s, task, solution, next)
				}
			}
		}
	}
	e.invalidateApplications(s)
}
func taskHasActiveMembers(s State, task Investigation) bool {
	for _, link := range s.Memberships {
		if link.InvestigationID != task.ID || link.State != "tentative" || link.IssueID != task.IssueID {
			continue
		}
		t, ok := s.Tickets[link.TicketID]
		if ok && !t.Private && !t.Closed && s.Latest[link.TicketID] == task.RunID && investigationSignature(t) == investigationSignature(s.Snapshots[link.SnapshotID].Ticket) && investigationScope(t) == s.Groups[task.GroupID].ScopeHash {
			return true
		}
	}
	return false
}
func availableTask(s State, task Investigation, cutoff time.Time) bool {
	if task.IssueID != "" {
		if _, exists := s.Issues[task.IssueID]; !exists {
			return false
		}
	}
	if task.CreatedAt.After(cutoff) || task.Contract.ObservationCutoff.After(cutoff) {
		return false
	}
	run := s.Runs[task.RunID]
	source, ok := s.Tickets[run.TicketID]
	if !ok || source.Private || run.WorkflowVersion != WorkflowVersion {
		return false
	}
	switch task.State {
	case "queued", "running", "investigating", "awaiting_configuration", "failed", "awaiting_information", "rejected":
		return true
	case "agent_proposed", "trusted", "reuse_proposed", "applied":
		solution, ok := taskSolution(s, task)
		return ok && !s.Tickets[solution.SourceTicketID].Private && (solution.State == "proposed" || solution.State == "trusted")
	}
	return false
}

// Unstarted work loses its purpose when every member closes, changes or receives
// a replacement investigation. Running work retains its usage audit on completion.
func supersedeInactiveTask(s *State, runID string, now time.Time) {
	task, ok := s.Investigations[s.Runs[runID].InvestigationID]
	if !ok || task.InvocationStarted || task.State != "queued" && task.State != "awaiting_configuration" || taskHasActiveMembers(*s, task) {
		return
	}
	task.State, task.UpdatedAt = "superseded", now
	s.Investigations[task.ID] = task
}

func (e *Engine) newRun(s *State, t Ticket, snap TicketSnapshot, cutoff, now time.Time) Run {
	return Run{ID: randomID("run_"), TicketID: t.ID, ProjectID: t.ProjectID, WorkflowVersion: WorkflowVersion, RuleVersion: e.Config.Version, Input: t, InputHash: diagnosticHash(t), SnapshotID: snap.ID, ObservationCutoff: cutoff, CreatedAt: now, UpdatedAt: now}
}

func recordRun(s *State, run Run) Receipt {
	s.Runs[run.ID], s.Latest[run.TicketID] = run, run.ID
	return Receipt{TicketID: run.TicketID, RunID: run.ID, Action: "created"}
}

func recordInvestigationLink(s *State, link IssueTicketLink) {
	for id, old := range s.Memberships {
		if old.TicketID == link.TicketID && old.State == "tentative" && old.InvestigationID != link.InvestigationID {
			old.State, old.AvailableAt = "detached", link.AvailableAt
			old.Version++
			s.Memberships[id] = old
			s.MembershipHistory = append(s.MembershipHistory, old)
		}
	}
	link.Version = 1
	s.Memberships[link.ID] = link
	s.MembershipHistory = append(s.MembershipHistory, link)
}

func (e *Engine) enqueue(s *State, t Ticket, now time.Time, force bool) Receipt {
	prepareInvestigations(s)
	snap, exists := s.Snapshots[s.LatestSnapshot[t.ID]]
	cutoff := snap.Cutoff
	if !exists {
		cutoff = now
	}
	if !exists {
		run := e.newRun(s, t, snap, cutoff, now)
		run.Route, run.State = "manual_forward", "awaiting_forward"
		run.MissingInformation = []string{"Forward a new immutable snapshot before diagnosis."}
		return recordRun(s, run)
	}
	if !force {
		if old, ok := s.Runs[s.Latest[t.ID]]; ok && old.WorkflowVersion == WorkflowVersion && old.InputHash == diagnosticHash(t) && old.RuleVersion == e.Config.Version {
			if (s.IssueOverrides[t.ID] == "" || old.IssueID == s.IssueOverrides[t.ID]) && (old.InvestigationID == "" || availableTask(*s, s.Investigations[old.InvestigationID], cutoff)) {
				return Receipt{TicketID: t.ID, RunID: old.ID, Action: "cached"}
			}
		}
	}
	signature := investigationSignature(t)
	groupID := "group_" + signature
	if override := s.IssueOverrides[t.ID]; override != "" {
		groupID = "group_" + hash(struct{ Signature, Issue string }{signature, override})
	}
	group, ok := s.Groups[groupID]
	if !ok {
		group = InvestigationGroup{ID: groupID, ProjectID: t.ProjectID, Signature: signature, ScopeHash: investigationScope(t), CreatedAt: cutoff}
	}
	if !force && availableTask(*s, s.Investigations[group.CurrentTaskID], cutoff) {
		task := s.Investigations[group.CurrentTaskID]
		link := IssueTicketLink{ID: randomID("link_"), IssueID: group.IssueID, TicketID: t.ID, SnapshotID: snap.ID, InvestigationID: task.ID, State: "tentative", Basis: "exact diagnostic and context signature; common causality unconfirmed", AvailableAt: cutoff}
		recordInvestigationLink(s, link)
		s.Latest[t.ID] = task.RunID
		return Receipt{TicketID: t.ID, RunID: task.RunID, Action: "linked_investigation"}
	}
	// A group deduplicates work; it does not classify the problem. Only an
	// explicit human assignment can predate this task's accepted Agent result.
	group.IssueID = s.IssueOverrides[t.ID]
	id := randomID("investigation_")
	run := e.newRun(s, t, snap, cutoff, now)
	runID := run.ID
	contract := TaskContract{Objective: "Routing Agent judges the ticket against the local Issue directory. Reuse a justified solution or obtain read-only engineering findings and an issue_request; Routing Agent then decides Issue grouping. Request essential missing information rather than inventing a result.", SnapshotID: snap.ID, InputHash: snap.Hash, ObservationCutoff: cutoff, IssueID: group.IssueID, ReadOnly: true, CompletionCriteria: []string{"Address the reported behavior using supplied historical evidence", "Engineering submits a grouping request; Routing Agent decides assignment", "Do not modify product source or close customer tickets"}}
	task := Investigation{ID: id, GroupID: groupID, IssueID: group.IssueID, ProjectID: t.ProjectID, RunID: runID, State: "queued", Contract: contract, CreatedAt: cutoff, UpdatedAt: now}
	run.InvestigationID, run.IssueID, run.Route = id, group.IssueID, "engineering_analysis"
	group.CurrentTaskID = id
	s.Groups[groupID] = group
	s.Investigations[id] = task
	s.Runs[runID] = run
	s.Latest[t.ID] = runID
	link := IssueTicketLink{ID: randomID("link_"), IssueID: group.IssueID, TicketID: t.ID, SnapshotID: snap.ID, InvestigationID: id, State: "tentative", Basis: "new exact-signature investigation", AvailableAt: cutoff}
	recordInvestigationLink(s, link)
	return Receipt{TicketID: t.ID, RunID: runID, Action: "created"}
}

func (e *Engine) ReviewSolution(id string, review SolutionReview) (SolutionRevision, error) {
	var result SolutionRevision
	err := e.update(func(s *State) error {
		task, ok := s.Investigations[id]
		if !ok {
			return ErrNotFound
		}
		if _, exists := s.Issues[task.IssueID]; !exists {
			return fmt.Errorf("%w: engineering issue was removed", ErrConflict)
		}
		touchIssue(s, task.IssueID, e.Now())
		if strings.TrimSpace(review.Reviewer) == "" || !contains([]string{"approve", "reject", "withdraw"}, review.Action) {
			return fmt.Errorf("%w: reviewer and valid action required", ErrInvalid)
		}
		if task.Application != nil {
			var err error
			result, err = e.reviewApplication(s, task, review)
			return err
		}
		old, ok := s.Solutions[task.SolutionID]
		if !ok {
			return ErrNotFound
		}
		if review.ExpectedSolutionID != old.ID {
			return fmt.Errorf("%w: solution revision changed", ErrConflict)
		}
		if review.Action == "approve" && (strings.TrimSpace(review.Resolution.Cause) == "" || strings.TrimSpace(review.Resolution.Solution) == "" || strings.TrimSpace(review.Resolution.Validation) == "") {
			return fmt.Errorf("%w: cause, solution and verification required", ErrInvalid)
		}
		source := s.Tickets[s.Runs[task.RunID].TicketID]
		group := s.Groups[task.GroupID]
		if source.Private || s.Tickets[old.SourceTicketID].Private || group.CurrentTaskID != task.ID || investigationScope(source) != group.ScopeHash {
			return ErrConflict
		}
		result = old
		result.Origin = "engineer"
		result.Reviewer = review.Reviewer
		result.Note = review.Note
		switch review.Action {
		case "approve":
			result.State = "trusted"
			result.Resolution = review.Resolution
		case "reject":
			result.State = "rejected"
		case "withdraw":
			result.State = "withdrawn"
		}
		if result.State == old.State && sameResolution(result.Resolution, old.Resolution) {
			result = old
			return nil
		}
		result = e.publishSolution(s, task, old, result)
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
