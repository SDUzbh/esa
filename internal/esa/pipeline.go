package esa

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EngineeringConcurrency is the global hard limit reserved transactionally across workers.
const EngineeringConcurrency = 3

var errEngineeringCapacity = errors.New("engineering capacity is occupied")

func activeInvestigation(task Investigation) bool {
	return task.State == InvestigationRunning || task.State == InvestigationInvestigating
}

// parked reports a task that waits for an engineering slot without holding a
// worker or a lease. Its routed report and frozen provider input are durable.
func parked(task Investigation) bool {
	return task.State == InvestigationInvestigating && task.Phase == "waiting_engineering" && task.Owner == ""
}

// engineeringActive counts reserved engineering slots, including single-call runs.
func engineeringActive(s State) int {
	active := 0
	for _, task := range s.investigationsIn(InvestigationRunning, InvestigationInvestigating) {
		if task.Phase == "investigating" || task.Phase == "" {
			active++
		}
	}
	return active
}

// fairParked orders parked tasks oldest first, rotating across projects.
func fairParked(s State) []Investigation {
	var rows []Investigation
	for _, task := range s.investigationsIn(InvestigationInvestigating) {
		if parked(task) {
			rows = append(rows, task)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].WaitingSince.Equal(rows[j].WaitingSince) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].WaitingSince.Before(rows[j].WaitingSince)
	})
	rotateProjects(rows, s.LastClaimedProject)
	return rows
}

// Intake routing is serialized per project until its claim is committed. The
// durable phase/lease is shared by all workers, including other ESA processes.
func routingBusy(s State, project string) bool {
	for _, task := range s.investigationsIn(InvestigationRunning) {
		if task.ProjectID == project && task.Phase == "routing" {
			return true
		}
	}
	return false
}

func ownsInvestigation(current, claimed Investigation) bool {
	return activeInvestigation(current) && current.Owner == claimed.Owner && current.Epoch == claimed.Epoch
}

// stage gives each provider stage its own timeout_seconds deadline, so time
// spent in earlier stages or waiting for capacity cannot shorten it.
func (e *Engine) stage(ctx context.Context, provider PipelineAnalyzer, input AnalyzeRequest, name string) (Report, error) {
	stageCtx, cancel := context.WithTimeout(ctx, time.Duration(e.Config.Budget.TimeoutSeconds)*time.Second)
	defer cancel()
	report, err := provider.AnalyzeStage(stageCtx, input, name)
	if err == nil && stageCtx.Err() != nil {
		err = stageCtx.Err()
	}
	return report, err
}

// errParked tells ProcessOne that the task now waits without its worker.
var errParked = errors.New("task parked until an engineering slot frees")

func queueTimeout(seconds int) error {
	// Routing usage is exact; nothing uncertain was spent on engineering.
	return fmt.Errorf("engineering capacity stayed occupied for %d seconds; no engineering call was made. Forward the ticket again to retry", seconds)
}

// pipeline runs route, then reserves an engineering slot or parks the task.
// It persists an investigating claim before releasing the project routing gate.
// A parked task returns errParked; ProcessOne later resumes it with engineer.
func (e *Engine) pipeline(ctx context.Context, provider PipelineAnalyzer, input AnalyzeRequest, task Investigation, started time.Time) (Report, error) {
	routed, err := e.stage(ctx, provider, input, "route")
	if err != nil {
		return routed, err
	}
	decision := routed.Routing
	if decision == nil ||
		decision.Usage.Calls != 1 ||
		routed.Usage.Calls != 1 ||
		strings.TrimSpace(decision.Reason) == "" ||
		len(routed.Findings) != 0 ||
		routed.AssignmentRouting != nil ||
		routed.IssueRequest != nil {
		return routed, fmt.Errorf("invalid intake routing stage")
	}
	if decision.Action != "analyze" {
		return routed, nil
	}
	if decision.RecordPath != "" || decision.RecordHash != "" || routed.Issue != nil || len(routed.Evidence) != 0 {
		return routed, fmt.Errorf("intake analysis cannot assign or publish a solution")
	}
	// Persist both the route audit and the visible claim before another intake
	// router can inspect this project. No Issue is created before diagnosis.
	// Capacity is reserved transactionally, not just in a process-local channel.
	// A task never overtakes one already parked, so slots go out in order.
	parkedNow := false
	err = e.update(func(s *State) error {
		current := s.Investigations.Get(task.ID)
		if !ownsInvestigation(current, task) || !taskHasActiveMembers(*s, current) {
			return ErrConflict
		}
		run := s.Runs.Get(task.RunID)
		run.Report, run.UpdatedAt = routed, e.Now()
		current.State, current.Phase, current.UpdatedAt = InvestigationInvestigating, "investigating", e.Now()
		parkedNow = engineeringActive(*s) >= EngineeringConcurrency || len(fairParked(*s)) > 0
		if parkedNow {
			frozen := input
			run.PipelineInput = &frozen
			current.Phase, current.Owner, current.LeaseUntil = "waiting_engineering", "", time.Time{}
			current.WaitingSince = time.Now().UTC()
			current.ElapsedMS = time.Since(started).Milliseconds()
		}
		s.Investigations.Put(task.ID, current)
		s.Runs.Put(run.ID, run)
		return nil
	})
	if err != nil {
		return routed, err
	}
	// The project routing gate is open again, and a parked task frees its worker.
	e.Store.signalWork()
	if parkedNow {
		return routed, errParked
	}
	return e.engineer(ctx, provider, input, task, routed)
}

// engineer runs the diagnosis in a reserved slot, then assignment routing.
// Engineering capacity is released before assignment; Routing cannot rewrite findings.
func (e *Engine) engineer(ctx context.Context, provider PipelineAnalyzer, input AnalyzeRequest, task Investigation, routed Report) (Report, error) {
	engineered, err := e.stage(ctx, provider, input, "engineer")
	if err != nil {
		return uncertainStage(engineered, routed), err
	}
	if engineered.Routing == nil ||
		hash(*engineered.Routing) != hash(*routed.Routing) ||
		engineered.Usage.Calls != 2 ||
		engineered.Issue != nil ||
		engineered.AssignmentRouting != nil {
		return engineered, fmt.Errorf("engineering cannot assign Issues or replace the routing decision")
	}
	if len(engineered.Findings) == 0 && len(engineered.Evidence) == 0 {
		return engineered, nil
	}
	if engineered.IssueRequest == nil || strings.TrimSpace(engineered.IssueRequest.Title) == "" || strings.TrimSpace(engineered.IssueRequest.Reason) == "" {
		return engineered, fmt.Errorf("engineering grouping request is required")
	}
	// Runtime cleanup has finished when the engineering stage returns. Release
	// its durable slot before Routing Agent reviews the grouping request.
	err = e.update(func(s *State) error {
		current := s.Investigations.Get(task.ID)
		if !ownsInvestigation(current, task) || !taskHasActiveMembers(*s, current) {
			return ErrConflict
		}
		current.Phase, current.UpdatedAt = "assignment", e.Now()
		s.Investigations.Put(task.ID, current)
		run := s.Runs.Get(task.RunID)
		run.Report, run.UpdatedAt = engineered, e.Now()
		s.Runs.Put(run.ID, run)
		return nil
	})
	if err != nil {
		return engineered, err
	}
	e.Store.signalWork()
	assigned, err := e.stage(ctx, provider, input, "assign")
	if err != nil {
		return uncertainStage(assigned, engineered), err
	}
	if assignmentRewroteDiagnosis(assigned, engineered, routed) {
		return assigned, fmt.Errorf("assignment routing cannot rewrite the engineering diagnosis")
	}
	return assigned, nil
}

func uncertainStage(result, previous Report) Report {
	if result.Usage.Calls == 0 {
		// The stage may have started despite a transport failure.
		result = previous
		result.Usage.Calls++
		result.Usage.TokensReported = false
	}
	return result
}

// Routing may join only an exact record it read. The pending record is a hint;
// the transaction rechecks current scope, visibility, membership and lease.
func (e *Engine) joinInvestigation(s *State, task *Investigation, report Report, records []IssueRecord) error {
	decision := report.Routing
	var selected *IssueRecord
	for i := range records {
		r := &records[i]
		if r.Kind == "investigating" && recordPath(*r) == decision.RecordPath && issueRecordHash(*r) == decision.RecordHash {
			selected = r
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("%w: join requires an eligible investigating record", ErrInvalid)
	}
	target, ok := s.Investigations.Lookup(selected.InvestigationID)
	source := s.Tickets.Get(selected.SourceTicketID)
	input := s.Runs.Get(task.RunID).Input
	if joinTargetChanged(s, *task, target, ok, selected, source, input) {
		return fmt.Errorf("%w: investigation source or scope changed", ErrConflict)
	}
	live := activeInvestigation(target) &&
		(target.LeaseUntil.After(e.Now()) || parked(target)) &&
		(target.Phase == "waiting_engineering" || target.Phase == "investigating" || target.Phase == "assignment")
	// Completion can race the routing turn. Share the completed investigation
	// rather than starting another analysis or claiming a future solution.
	if !live && target.State != InvestigationAgentProposed && target.State != InvestigationTrusted {
		return fmt.Errorf("%w: investigation is no longer available", ErrConflict)
	}
	for id, link := range s.Memberships.All() {
		if link.InvestigationID != task.ID || link.State != MembershipTentative {
			continue
		}
		if s.IssueOverrides.Get(link.TicketID) != "" && s.IssueOverrides.Get(link.TicketID) != target.IssueID {
			return fmt.Errorf("%w: reviewed membership cannot be overridden", ErrConflict)
		}
		link.InvestigationID, link.IssueID = target.ID, target.IssueID
		link.Basis, link.AvailableAt = "Routing Agent joined investigation: "+decision.Reason, e.Now()
		link.Version++
		s.Memberships.Put(id, link)
		s.MembershipHistory = append(s.MembershipHistory, link)
		s.Latest.Put(link.TicketID, target.RunID)
	}
	for id, group := range s.Groups.All() {
		if group.CurrentTaskID == task.ID {
			group.CurrentTaskID, group.IssueID = target.ID, target.IssueID
			s.Groups.Put(id, group)
		}
	}
	task.State, task.JoinedInvestigationID = InvestigationJoined, target.ID
	return nil
}

// joinTargetChanged reports whether the investigation a routing join names
// no longer matches what the frozen directory showed: gone, the same task,
// another project, a private or closed source, another run, superseded,
// without active members, a changed source or scope, another Issue, or a
// later cutoff.
func joinTargetChanged(s *State, task, target Investigation, ok bool, selected *IssueRecord, source, input Ticket) bool {
	return !ok ||
		target.ID == task.ID ||
		target.ProjectID != task.ProjectID ||
		source.Private ||
		source.Closed ||
		target.RunID != selected.RunID ||
		s.Groups.Get(target.GroupID).CurrentTaskID != target.ID ||
		!taskHasActiveMembers(*s, target) ||
		investigationSignature(source) != investigationSignature(s.Runs.Get(target.RunID).Input) ||
		investigationScope(input) != selected.ScopeHash ||
		target.Contract.IssueID != task.Contract.IssueID ||
		target.Contract.ObservationCutoff.After(task.Contract.ObservationCutoff)
}

// assignmentRewroteDiagnosis reports whether the assignment stage changed
// anything the engineering or routing stage produced; assignment may only
// group it.
func assignmentRewroteDiagnosis(assigned, engineered, routed Report) bool {
	return assigned.Usage.Calls != 3 ||
		hash(assigned.Findings) != hash(engineered.Findings) ||
		hash(assigned.Evidence) != hash(engineered.Evidence) ||
		hash(assigned.IssueRequest) != hash(engineered.IssueRequest) ||
		assigned.Summary != engineered.Summary ||
		hash(assigned.Recommendations) != hash(engineered.Recommendations) ||
		hash(assigned.VerificationSteps) != hash(engineered.VerificationSteps) ||
		assigned.Routing == nil ||
		hash(*assigned.Routing) != hash(*routed.Routing)
}
