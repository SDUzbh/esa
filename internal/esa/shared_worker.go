package esa

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (e *Engine) recoverShared(s *State) error {
	for id, task := range s.Investigations {
		if task.State == "running" && !task.LeaseUntil.After(e.Now()) {
			task.State = "failed"
			task.Epoch++
			task.UpdatedAt = e.Now()
			s.Investigations[id] = task
			r := s.Runs[task.RunID]
			r.State = "failed"
			r.Error = "Investigation lease expired. Inspect the invocation audit before an explicit retry; spend is uncertain."
			r.UpdatedAt = e.Now()
			s.Runs[r.ID] = r
		}
	}
	for id, r := range s.Runs {
		if r.State == "queued" && r.WorkflowVersion != "5" {
			r.State = "awaiting_forward"
			r.Route = "manual_forward"
			s.Runs[id] = r
		}
	}
	return nil
}

func validateSharedReport(report Report, input AnalyzeRequest) error {
	if len(report.Findings) > 50 || len(report.Evidence) > 100 || len(report.Summary) > 16384 {
		return fmt.Errorf("provider report exceeds bounded finding/evidence policy")
	}
	if strings.TrimSpace(report.Summary) == "" || len(report.Evidence) == 0 && (len(report.Findings) > 0 || len(report.Limitations) == 0) {
		return fmt.Errorf("provider must return a summary and evidence, or explicit limitations")
	}
	for _, ev := range report.Evidence {
		if ev.Source == "" || ev.Reference == "" || strings.TrimSpace(ev.Quote) == "" {
			return fmt.Errorf("incomplete evidence")
		}
	}
	if report.Issue != nil && report.Issue.ExistingID != "" {
		found := false
		for _, issue := range input.KnownIssues {
			if issue.ID == report.Issue.ExistingID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("issue assignment is outside the supplied registry")
		}
	}
	if report.Usage.Calls < 0 || report.Usage.Calls > input.Budget.MaxCalls || report.Usage.InputTokens < 0 || report.Usage.OutputTokens < 0 {
		return fmt.Errorf("invalid reported usage")
	}
	for _, finding := range report.Findings {
		if finding.Commit == "" || finding.File == "" || finding.StartLine < 1 || finding.EndLine < finding.StartLine || finding.EndLine-finding.StartLine >= 200 || strings.TrimSpace(finding.Problem) == "" || strings.TrimSpace(finding.Solution) == "" || len(finding.Evidence) == 0 || len(finding.Evidence) > 100 {
			return fmt.Errorf("finding lacks a pinned source location and evidence")
		}
	}
	return nil
}

func (e *Engine) processSharedOne(ctx context.Context, analyzer Analyzer) (bool, error) {
	if !e.Store.needsSharedWork(e.Now(), analyzer != nil && e.Config.Budget.MaxCalls > 0) {
		return false, nil
	}
	var task Investigation
	var run Run
	found := false
	err := e.Store.Update(func(s *State) error {
		found = false
		task = Investigation{}
		run = Run{}
		if err := e.recoverShared(s); err != nil {
			return err
		}
		active := map[string]int{}
		for _, item := range s.Investigations {
			if item.State == "running" {
				active[item.ProjectID]++
			}
		}
		for _, candidate := range fairInvestigations(*s) {
			if candidate.State != "queued" && !(candidate.State == "awaiting_configuration" && !candidate.InvocationStarted && analyzer != nil && e.Config.Budget.MaxCalls > 0) {
				continue
			}
			if active[candidate.ProjectID] >= e.Config.V2.MaxActivePerProject {
				continue
			}
			group := s.Groups[candidate.GroupID]
			if group.CurrentTaskID != candidate.ID || !taskHasActiveMembers(*s, candidate) {
				candidate.State = "superseded"
				s.Investigations[candidate.ID] = candidate
				r := s.Runs[candidate.RunID]
				r.State = "superseded"
				s.Runs[r.ID] = r
				continue
			}
			task = candidate
			run = s.Runs[task.RunID]
			found = true
			if analyzer == nil || e.Config.Budget.MaxCalls == 0 {
				task.State = "awaiting_configuration"
				run.State = "awaiting_configuration"
				run.Error = "Engineering provider is not configured; no model invocation occurred."
			} else {
				task.State = "running"
				task.Owner = e.workerID
				task.Epoch++
				task.InvocationStarted = true
				task.HeartbeatAt = e.Now()
				task.LeaseUntil = e.Now().Add(time.Duration(e.Config.V2.LeaseSeconds) * time.Second)
				run.State = "running"
				run.Report.Usage.Calls = 1
				s.LastClaimedProject = candidate.ProjectID
			}
			task.UpdatedAt = e.Now()
			run.UpdatedAt = e.Now()
			s.Investigations[task.ID] = task
			s.Runs[run.ID] = run
			break
		}
		return nil
	})
	if err != nil || !found || task.State != "running" {
		return found, err
	}
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(e.Config.Budget.TimeoutSeconds)*time.Second)
	defer cancel()
	done := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(time.Duration(e.Config.V2.LeaseSeconds) * time.Second / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-callCtx.Done():
				return
			case <-ticker.C:
				if err := e.Store.Update(func(s *State) error {
					current := s.Investigations[task.ID]
					if current.Owner != task.Owner || current.Epoch != task.Epoch || current.State != "running" {
						return ErrConflict
					}
					current.HeartbeatAt = e.Now()
					current.LeaseUntil = e.Now().Add(time.Duration(e.Config.V2.LeaseSeconds) * time.Second)
					s.Investigations[task.ID] = current
					return nil
				}); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	input := AnalyzeRequest{RunID: run.ID, Ticket: run.Input, Budget: e.Config.Budget, ReadOnly: true, Contract: &task.Contract, KnownIssues: []IssueBrief{{ID: task.IssueID, Title: run.Input.Subject}}}
	s := e.Store.Snapshot()
	for _, match := range run.Matches {
		for _, c := range CasesAt(eligibleCaseHistory(s, run.Input), task.Contract.ObservationCutoff) {
			if c.ID == match.CaseID && c.Version == match.CaseVersion {
				input.Cases = append(input.Cases, c)
			}
		}
	}
	report, callErr := analyzer.Analyze(callCtx, input)
	if callCtx.Err() != nil && callErr == nil {
		callErr = callCtx.Err()
	}
	close(done)
	<-heartbeatDone
	if callErr == nil {
		callErr = validateSharedReport(report, input)
	}
	report.AgentClassification = nil
	report.Envelope = reportEnvelope(task, e.Now(), time.Since(started).Milliseconds())
	report.Envelope.Plan = report.Plan
	assessReport(&report, input)
	report.Usage.Calls = 1
	AddAlerts(&report, TokenAlerts(report.Usage, e.Config.Budget)...)
	if !report.Usage.TokensReported {
		report.Limitations = append(report.Limitations, "Provider usage is unknown; it must not be interpreted as zero.")
	}
	err = e.Store.Update(func(s *State) error {
		current := s.Investigations[task.ID]
		if current.Owner != task.Owner || current.Epoch != task.Epoch || current.State != "running" {
			return fmt.Errorf("%w: stale investigation result rejected", ErrConflict)
		}
		r := s.Runs[run.ID]
		if !taskHasActiveMembers(*s, current) || s.Groups[current.GroupID].CurrentTaskID != current.ID {
			current.State = "superseded"
			r.State = "superseded"
		} else if callErr != nil {
			current.State = "failed"
			r.State = "failed"
			r.Error = callErr.Error()
		} else {
			current.State = "agent_proposed"
			r.State = "awaiting_review"
			r.Report = report
			r.Error = ""
			if len(report.Findings) == 0 && len(report.Evidence) == 0 {
				current.State = "awaiting_information"
				r.State = "awaiting_information"
			}
			if current.State == "agent_proposed" {
				group := s.Groups[current.GroupID]
				solution := SolutionRevision{ID: randomID("solution_"), IssueID: current.IssueID, InvestigationID: current.ID, ProjectID: current.ProjectID, ScopeHash: group.ScopeHash, SourceTicketID: run.TicketID, Origin: "agent", State: "proposed", Revision: 1, Report: report, AvailableAt: e.Now(), RecordedAt: time.Now().UTC()}
				s.Solutions[solution.ID] = solution
				current.SolutionID = solution.ID
			}
		}
		current.ElapsedMS = time.Since(started).Milliseconds()
		current.UpdatedAt = e.Now()
		current.LeaseUntil = time.Time{}
		r.UpdatedAt = e.Now()
		s.Investigations[current.ID] = current
		s.Runs[r.ID] = r
		return nil
	})
	return true, err
}
