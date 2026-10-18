package esa

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func (e *Engine) Version() string { return WorkflowVersion }

// captureSnapshot stores the event's ticket as an immutable snapshot and
// returns its ID.
func captureSnapshot(s *State, event Event) string {
	id := snapshotIDFor(event.ID)
	t := event.Ticket
	s.Snapshots.Put(id, TicketSnapshot{ID: id, Hash: hash(t), EventID: event.ID, Ticket: t, EffectiveAt: t.UpdatedAt, AvailableAt: event.ForwardedAt, RecordedAt: time.Now().UTC(), Cutoff: event.ForwardedAt})
	s.LatestSnapshot.Put(t.ID, id)
	return id
}

func snapshotIDFor(eventID string) string { return "snapshot_" + hash(eventID) }

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
	for _, id := range s.membershipsOfTicket(t.ID) {
		link := s.Memberships.Get(id)
		if link.State != MembershipTentative {
			continue
		}
		old := s.Snapshots.Get(link.SnapshotID).Ticket
		if t.Private || t.Closed || investigationSignature(old) != investigationSignature(t) {
			link.State = MembershipDetached
			link.Version++
			s.Memberships.Put(id, link)
			s.MembershipHistory = append(s.MembershipHistory, link)
		}
	}
	if t.Private {
		for id, task := range s.Investigations.All() {
			run := s.Runs.Get(task.RunID)
			if (run.TicketID == t.ID || taskSourceTicket(*s, task) == t.ID) && task.State != InvestigationWithdrawn {
				task.State = InvestigationWithdrawn
				task.UpdatedAt = e.Now()
				s.Investigations.Put(id, task)
				if solution, ok := s.Solutions.Lookup(task.SolutionID); ok && solution.State != SolutionWithdrawn {
					next := solution
					next.State = SolutionWithdrawn
					next.Note = "The source ticket became private"
					e.publishSolution(s, task, solution, next)
				}
			}
		}
	}
	e.invalidateApplications(s)
}
func taskHasActiveMembers(s State, task Investigation) bool {
	for _, id := range s.membershipsOfInvestigation(task.ID) {
		link := s.Memberships.Get(id)
		if link.State != MembershipTentative || link.IssueID != task.IssueID {
			continue
		}
		t, ok := s.Tickets.Lookup(link.TicketID)
		if ok && !t.Private && !t.Closed && s.Latest.Get(link.TicketID) == task.RunID && investigationSignature(t) == investigationSignature(s.Snapshots.Get(link.SnapshotID).Ticket) && investigationScope(t) == s.Groups.Get(task.GroupID).ScopeHash {
			return true
		}
	}
	return false
}
func availableTask(s State, task Investigation, cutoff time.Time) bool {
	if task.IssueID != "" {
		if _, exists := s.Issues.Lookup(task.IssueID); !exists {
			return false
		}
	}
	if task.CreatedAt.After(cutoff) || task.Contract.ObservationCutoff.After(cutoff) {
		return false
	}
	run := s.Runs.Get(task.RunID)
	source, ok := s.Tickets.Lookup(run.TicketID)
	if !ok || source.Private || run.WorkflowVersion != WorkflowVersion {
		return false
	}
	switch task.State {
	case InvestigationQueued, InvestigationRunning, InvestigationInvestigating, InvestigationAwaitingConfiguration, InvestigationFailed, InvestigationAwaitingInformation, InvestigationReturnedToSupport, InvestigationRejected:
		return true
	case InvestigationAgentProposed, InvestigationTrusted, InvestigationReuseProposed, InvestigationApplied:
		solution, ok := taskSolution(s, task)
		return ok && !s.Tickets.Get(solution.SourceTicketID).Private && (solution.State == SolutionProposed || solution.State == SolutionTrusted)
	}
	return false
}

// Unstarted work loses its purpose when every member closes, changes or receives
// a replacement investigation. Running work retains its usage audit on completion.
func supersedeInactiveTask(s *State, runID string, now time.Time) {
	task, ok := s.Investigations.Lookup(s.Runs.Get(runID).InvestigationID)
	if !ok || task.InvocationStarted || task.State != InvestigationQueued && task.State != InvestigationAwaitingConfiguration || taskHasActiveMembers(*s, task) {
		return
	}
	task.State, task.UpdatedAt = InvestigationSuperseded, now
	s.Investigations.Put(task.ID, task)
}

func (e *Engine) newRun(s *State, t Ticket, snap TicketSnapshot, cutoff, now time.Time) Run {
	return Run{ID: randomID("run_"), TicketID: t.ID, ProjectID: t.ProjectID, WorkflowVersion: WorkflowVersion, RuleVersion: e.Config.Version, Input: t, InputHash: diagnosticHash(t), SnapshotID: snap.ID, ObservationCutoff: cutoff, CreatedAt: now, UpdatedAt: now}
}

func recordRun(s *State, run Run) Receipt {
	s.Runs.Put(run.ID, run)
	s.Latest.Put(run.TicketID, run.ID)
	return Receipt{TicketID: run.TicketID, RunID: run.ID, Action: "created"}
}

func recordInvestigationLink(s *State, link IssueTicketLink) {
	for _, id := range s.membershipsOfTicket(link.TicketID) {
		old := s.Memberships.Get(id)
		if old.State == MembershipTentative && old.InvestigationID != link.InvestigationID {
			old.State, old.AvailableAt = MembershipDetached, link.AvailableAt
			old.Version++
			s.Memberships.Put(id, old)
			s.MembershipHistory = append(s.MembershipHistory, old)
		}
	}
	link.Version = 1
	s.Memberships.Put(link.ID, link)
	s.MembershipHistory = append(s.MembershipHistory, link)
}

func (e *Engine) enqueue(s *State, t Ticket, now time.Time, force bool) Receipt {
	snap, exists := s.Snapshots.Lookup(s.LatestSnapshot.Get(t.ID))
	cutoff := snap.Cutoff
	if !exists {
		cutoff = now
	}
	if !exists {
		run := e.newRun(s, t, snap, cutoff, now)
		run.Route, run.State = "manual_forward", RunAwaitingForward
		run.MissingInformation = []string{"Forward a new immutable snapshot before diagnosis."}
		return recordRun(s, run)
	}
	if !force {
		if old, ok := s.Runs.Lookup(s.Latest.Get(t.ID)); ok && old.WorkflowVersion == WorkflowVersion && old.InputHash == diagnosticHash(t) && old.RuleVersion == e.Config.Version {
			if (s.IssueOverrides.Get(t.ID) == "" || old.IssueID == s.IssueOverrides.Get(t.ID)) && (old.InvestigationID == "" || availableTask(*s, s.Investigations.Get(old.InvestigationID), cutoff)) {
				return Receipt{TicketID: t.ID, RunID: old.ID, Action: "cached"}
			}
		}
	}
	signature := investigationSignature(t)
	groupID := "group_" + signature
	if override := s.IssueOverrides.Get(t.ID); override != "" {
		groupID = "group_" + hash(struct{ Signature, Issue string }{signature, override})
	}
	group, ok := s.Groups.Lookup(groupID)
	if !ok {
		group = InvestigationGroup{ID: groupID, ProjectID: t.ProjectID, Signature: signature, ScopeHash: investigationScope(t), CreatedAt: cutoff}
	}
	if !force && availableTask(*s, s.Investigations.Get(group.CurrentTaskID), cutoff) {
		task := s.Investigations.Get(group.CurrentTaskID)
		link := IssueTicketLink{ID: randomID("link_"), IssueID: group.IssueID, TicketID: t.ID, SnapshotID: snap.ID, InvestigationID: task.ID, State: MembershipTentative, Basis: "exact diagnostic and context signature; common causality unconfirmed", AvailableAt: cutoff}
		recordInvestigationLink(s, link)
		s.Latest.Put(t.ID, task.RunID)
		return Receipt{TicketID: t.ID, RunID: task.RunID, Action: "linked_investigation"}
	}
	// A group deduplicates work; it does not classify the problem. Only an
	// explicit human assignment can predate this task's accepted Agent result.
	group.IssueID = s.IssueOverrides.Get(t.ID)
	id := randomID("investigation_")
	run := e.newRun(s, t, snap, cutoff, now)
	runID := run.ID
	contract := TaskContract{Objective: "Routing Agent judges the ticket against the local Issue directory. Reuse a justified solution or obtain read-only engineering findings and an issue_request; Routing Agent then decides Issue grouping. Request essential missing information rather than inventing a result.", SnapshotID: snap.ID, InputHash: snap.Hash, ObservationCutoff: cutoff, IssueID: group.IssueID, ReadOnly: true, CompletionCriteria: []string{"Address the reported behavior using supplied historical evidence", "Engineering submits a grouping request; Routing Agent decides assignment", "Do not modify product source or close customer tickets"}}
	task := Investigation{ID: id, GroupID: groupID, IssueID: group.IssueID, ProjectID: t.ProjectID, RunID: runID, State: InvestigationQueued, Contract: contract, CreatedAt: cutoff, UpdatedAt: now}
	run.InvestigationID, run.IssueID, run.Route = id, group.IssueID, "engineering_analysis"
	group.CurrentTaskID = id
	s.Groups.Put(groupID, group)
	s.Investigations.Put(id, task)
	s.Runs.Put(runID, run)
	s.Latest.Put(t.ID, runID)
	link := IssueTicketLink{ID: randomID("link_"), IssueID: group.IssueID, TicketID: t.ID, SnapshotID: snap.ID, InvestigationID: id, State: MembershipTentative, Basis: "new exact-signature investigation", AvailableAt: cutoff}
	recordInvestigationLink(s, link)
	return Receipt{TicketID: t.ID, RunID: runID, Action: "created"}
}

func (e *Engine) ReviewSolution(id string, review SolutionReview) (SolutionRevision, error) {
	var result SolutionRevision
	err := e.update(func(s *State) error {
		task, ok := s.Investigations.Lookup(id)
		if !ok {
			return ErrNotFound
		}
		if _, exists := s.Issues.Lookup(task.IssueID); !exists {
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
		old, ok := s.Solutions.Lookup(task.SolutionID)
		if !ok {
			return ErrNotFound
		}
		if review.ExpectedSolutionID != old.ID {
			return fmt.Errorf("%w: solution revision changed", ErrConflict)
		}
		if review.Action == "approve" && (strings.TrimSpace(review.Resolution.Cause) == "" || strings.TrimSpace(review.Resolution.Solution) == "" || strings.TrimSpace(review.Resolution.Validation) == "") {
			return fmt.Errorf("%w: cause, solution and verification required", ErrInvalid)
		}
		source := s.Tickets.Get(s.Runs.Get(task.RunID).TicketID)
		group := s.Groups.Get(task.GroupID)
		if source.Private || s.Tickets.Get(old.SourceTicketID).Private || group.CurrentTaskID != task.ID || investigationScope(source) != group.ScopeHash {
			return ErrConflict
		}
		result = old
		result.Origin = "engineer"
		result.Reviewer = review.Reviewer
		result.Note = review.Note
		switch review.Action {
		case "approve":
			result.State = SolutionTrusted
			result.Resolution = review.Resolution
		case "reject":
			result.State = SolutionRejected
		case "withdraw":
			result.State = SolutionWithdrawn
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
	rows := make([]Investigation, 0, s.Investigations.Len())
	for _, task := range s.Investigations.All() {
		rows = append(rows, task)
	}
	sortByCreation(rows)
	return rows
}

func sortByCreation(rows []Investigation) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
}

// fairInvestigations orders the investigations in the given work states
// oldest first, rotating across projects.
func fairInvestigations(s State, states ...InvestigationState) []Investigation {
	rows := s.investigationsIn(states...)
	sortByCreation(rows)
	rotateProjects(rows, s.LastClaimedProject)
	return rows
}

// rotateProjects starts after the last claimed project and keeps each
// project's existing order.
func rotateProjects(rows []Investigation, last string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].ProjectID, rows[j].ProjectID
		if a == b {
			return false
		}
		aAfter, bAfter := a > last, b > last
		if aAfter != bAfter {
			return aAfter
		}
		return a < b
	})
}
