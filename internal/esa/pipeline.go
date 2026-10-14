package esa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const EngineeringConcurrency = 3

var errEngineeringCapacity = errors.New("engineering capacity is occupied")

func activeInvestigation(task Investigation) bool {
	return task.State == "running" || task.State == "investigating"
}

// Intake routing is serialized per project until its claim is committed. The
// durable phase/lease is shared by all workers, including other ESA processes.
func routingBusy(s State, project string) bool {
	for _, task := range s.Investigations {
		if task.ProjectID == project && task.State == "running" && task.Phase == "routing" {
			return true
		}
	}
	return false
}

func ownsInvestigation(current, claimed Investigation) bool {
	return activeInvestigation(current) && current.Owner == claimed.Owner && current.Epoch == claimed.Epoch
}

func (e *Engine) pipeline(ctx context.Context, provider PipelineAnalyzer, input AnalyzeRequest, task Investigation) (Report, error) {
	routed, err := provider.AnalyzeStage(ctx, input, "route")
	if err != nil {
		return routed, err
	}
	decision := routed.Routing
	if decision == nil || decision.Usage.Calls != 1 || routed.Usage.Calls != 1 || strings.TrimSpace(decision.Reason) == "" || len(routed.Findings) != 0 || routed.AssignmentRouting != nil || routed.IssueRequest != nil {
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
	err = e.update(func(s *State) error {
		current := s.Investigations[task.ID]
		if !ownsInvestigation(current, task) || !taskHasActiveMembers(*s, current) {
			return ErrConflict
		}
		current.State, current.Phase, current.UpdatedAt = "investigating", "waiting_engineering", e.Now()
		s.Investigations[task.ID] = current
		run := s.Runs[task.RunID]
		run.Report, run.UpdatedAt = routed, e.Now()
		s.Runs[run.ID] = run
		return nil
	})
	if err != nil {
		return routed, err
	}
	// Capacity is reserved transactionally, not just in a process-local channel.
	// Waiting tasks retain a heartbeat and are searchable as investigating.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		reserved := false
		err = e.update(func(s *State) error {
			reserved = false
			current := s.Investigations[task.ID]
			if !ownsInvestigation(current, task) || !taskHasActiveMembers(*s, current) {
				return ErrConflict
			}
			active := 0
			for _, other := range s.Investigations {
				if activeInvestigation(other) && (other.Phase == "investigating" || other.Phase == "") {
					active++
				}
			}
			if active >= EngineeringConcurrency {
				return errEngineeringCapacity
			}
			current.Phase, current.UpdatedAt = "investigating", e.Now()
			s.Investigations[current.ID] = current
			reserved = true
			return nil
		})
		if err != nil && !errors.Is(err, errEngineeringCapacity) {
			return routed, err
		}
		if reserved {
			break
		}
		select {
		case <-ctx.Done():
			return routed, ctx.Err()
		case <-ticker.C:
		}
	}
	engineered, err := provider.AnalyzeStage(ctx, input, "engineer")
	if err != nil {
		return uncertainStage(engineered, routed), err
	}
	if engineered.Routing == nil || hash(*engineered.Routing) != hash(*routed.Routing) || engineered.Usage.Calls != 2 || engineered.Issue != nil || engineered.AssignmentRouting != nil {
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
		current := s.Investigations[task.ID]
		if !ownsInvestigation(current, task) || !taskHasActiveMembers(*s, current) {
			return ErrConflict
		}
		current.Phase, current.UpdatedAt = "assignment", e.Now()
		s.Investigations[task.ID] = current
		run := s.Runs[task.RunID]
		run.Report, run.UpdatedAt = engineered, e.Now()
		s.Runs[run.ID] = run
		return nil
	})
	if err != nil {
		return engineered, err
	}
	assigned, err := provider.AnalyzeStage(ctx, input, "assign")
	if err != nil {
		return uncertainStage(assigned, engineered), err
	}
	if assigned.Usage.Calls != 3 || hash(assigned.Findings) != hash(engineered.Findings) || hash(assigned.Evidence) != hash(engineered.Evidence) || hash(assigned.IssueRequest) != hash(engineered.IssueRequest) || assigned.Summary != engineered.Summary || hash(assigned.Recommendations) != hash(engineered.Recommendations) || hash(assigned.VerificationSteps) != hash(engineered.VerificationSteps) || assigned.Routing == nil || hash(*assigned.Routing) != hash(*routed.Routing) {
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
	target, ok := s.Investigations[selected.InvestigationID]
	source := s.Tickets[selected.SourceTicketID]
	input := s.Runs[task.RunID].Input
	if !ok || target.ID == task.ID || target.ProjectID != task.ProjectID || source.Private || source.Closed || target.RunID != selected.RunID || s.Groups[target.GroupID].CurrentTaskID != target.ID || !taskHasActiveMembers(*s, target) || investigationSignature(source) != investigationSignature(s.Runs[target.RunID].Input) || investigationScope(input) != selected.ScopeHash || target.Contract.IssueID != task.Contract.IssueID || target.Contract.ObservationCutoff.After(task.Contract.ObservationCutoff) {
		return fmt.Errorf("%w: investigation source or scope changed", ErrConflict)
	}
	live := activeInvestigation(target) && target.LeaseUntil.After(e.Now()) && (target.Phase == "waiting_engineering" || target.Phase == "investigating" || target.Phase == "assignment")
	// Completion can race the routing turn. Share the completed investigation
	// rather than starting another analysis or claiming a future solution.
	if !live && target.State != "agent_proposed" && target.State != "trusted" {
		return fmt.Errorf("%w: investigation is no longer available", ErrConflict)
	}
	for id, link := range s.Memberships {
		if link.InvestigationID != task.ID || link.State != "tentative" {
			continue
		}
		if s.IssueOverrides[link.TicketID] != "" && s.IssueOverrides[link.TicketID] != target.IssueID {
			return fmt.Errorf("%w: reviewed membership cannot be overridden", ErrConflict)
		}
		link.InvestigationID, link.IssueID = target.ID, target.IssueID
		link.Basis, link.AvailableAt = "Routing Agent joined investigation: "+decision.Reason, e.Now()
		link.Version++
		s.Memberships[id] = link
		s.MembershipHistory = append(s.MembershipHistory, link)
		s.Latest[link.TicketID] = target.RunID
	}
	for id, group := range s.Groups {
		if group.CurrentTaskID == task.ID {
			group.CurrentTaskID, group.IssueID = target.ID, target.IssueID
			s.Groups[id] = group
		}
	}
	task.State, task.JoinedInvestigationID = "joined", target.ID
	return nil
}
