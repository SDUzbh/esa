package esa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) recoverInvestigations(s *State) error {
	for id, task := range s.Investigations {
		if activeInvestigation(task) && !task.LeaseUntil.After(e.Now()) {
			task.State = "failed"
			task.Epoch++
			task.UpdatedAt = e.Now()
			s.Investigations[id] = task
			r := s.Runs[task.RunID]
			r.Error = "Investigation lease expired. Inspect the invocation audit before an explicit retry; spend is uncertain."
			r.UpdatedAt = e.Now()
			s.Runs[r.ID] = r
		}
	}
	for id, r := range s.Runs {
		if r.State == "queued" && (r.WorkflowVersion != WorkflowVersion || r.Input.ForwardedBy == "" || r.Input.ForwardedAt.IsZero()) {
			r.State = "awaiting_forward"
			r.Route = "manual_forward"
			s.Runs[id] = r
			if task, ok := s.Investigations[r.InvestigationID]; ok {
				task.State = "awaiting_forward"
				s.Investigations[task.ID] = task
			}
		}
	}
	return nil
}

func validateReport(report Report, input AnalyzeRequest, records []IssueRecord) error {
	if len(report.AccessedIssueIDs) > IssueLimit {
		return fmt.Errorf("too many accessed issue IDs")
	}
	eligible := map[string]bool{}
	for _, record := range records {
		if record.IssueID != "" {
			eligible[record.IssueID] = true
		}
	}
	for _, id := range report.AccessedIssueIDs {
		if !eligible[id] {
			return fmt.Errorf("accessed issue is outside the supplied directory")
		}
	}
	if len(report.Findings) > 50 || len(report.Evidence) > 100 || len(report.Summary) > 16384 {
		return fmt.Errorf("provider report exceeds bounded finding/evidence policy")
	}
	if strings.TrimSpace(report.Summary) == "" || len(report.Evidence) == 0 && (len(report.Findings) > 0 || len(report.Limitations) == 0) {
		return fmt.Errorf("provider must return a summary and evidence, or explicit limitations")
	}
	awaitingGrouping := report.AssignmentRouting != nil && report.AssignmentRouting.Action == "request_information"
	if (len(report.Findings) > 0 || len(report.Evidence) > 0) && !awaitingGrouping && (report.Issue == nil || strings.TrimSpace(report.Issue.Title) == "" || strings.TrimSpace(report.Issue.Reason) == "") {
		return fmt.Errorf("supported analysis requires an Agent issue proposal with title and reason")
	}
	for _, ev := range report.Evidence {
		if ev.Source == "" || ev.Reference == "" || strings.TrimSpace(ev.Quote) == "" {
			return fmt.Errorf("incomplete evidence")
		}
	}
	if report.Issue != nil && report.Issue.ExistingID != "" {
		found := input.Contract != nil && report.Issue.ExistingID == input.Contract.IssueID
		for _, record := range records {
			found = found || record.IssueID == report.Issue.ExistingID
		}
		if !found {
			return fmt.Errorf("issue assignment is outside the supplied directory")
		}
	}
	maxCalls := input.Budget.MaxCalls
	if report.Routing != nil {
		maxCalls++
		if report.Routing.Action != "reuse" && report.Routing.Action != "analyze" && report.Routing.Action != "request_information" && report.Routing.Action != "join" && report.Routing.Action != "return_to_support" {
			return fmt.Errorf("invalid routing action")
		}
		if report.Routing.Action != "analyze" && (report.Usage.Calls > 1 || len(report.Findings) > 0) {
			return fmt.Errorf("reuse/clarification cannot invoke engineering or claim new findings")
		}
		if (report.Routing.Action == "request_information" || report.Routing.Action == "join" || report.Routing.Action == "return_to_support") && (len(report.Evidence) > 0 || report.Issue != nil) {
			return fmt.Errorf("clarification cannot publish a solution")
		}
		if report.Routing.Usage.Calls != 1 {
			return fmt.Errorf("invalid routing usage")
		}
		if report.Routing.Action == "analyze" && (len(report.Findings) > 0 || len(report.Evidence) > 0) && (report.IssueRequest == nil || strings.TrimSpace(report.IssueRequest.Title) == "" || strings.TrimSpace(report.IssueRequest.Reason) == "" || report.AssignmentRouting == nil) {
			return fmt.Errorf("engineering findings require a request and a Routing Agent grouping decision")
		}
	}
	if report.AssignmentRouting != nil {
		decision := report.AssignmentRouting
		if report.Routing == nil || report.Routing.Action != "analyze" || decision.Usage.Calls != 1 || strings.TrimSpace(decision.Reason) == "" || len(report.Findings) == 0 && len(report.Evidence) == 0 {
			return fmt.Errorf("invalid assignment routing stage")
		}
		maxCalls++
		switch decision.Action {
		case "assign", "create":
			if report.Issue == nil || report.Issue.Title != decision.Title || report.Issue.Reason != decision.Reason || (decision.Action == "assign") != (report.Issue.ExistingID != "") {
				return fmt.Errorf("Issue assignment does not match Routing Agent decision")
			}
			if decision.Action == "create" && (decision.RecordPath != "" || decision.RecordHash != "") {
				return fmt.Errorf("new Issue cannot select an existing record")
			}
		case "request_information":
			if report.Issue != nil || decision.RecordPath != "" || decision.RecordHash != "" || decision.Title != "" {
				return fmt.Errorf("grouping clarification cannot assign an Issue")
			}
		default:
			return fmt.Errorf("unsupported assignment routing action")
		}
	}
	if report.Usage.Calls < 0 || report.Usage.Calls > maxCalls || report.Usage.InputTokens < 0 || report.Usage.OutputTokens < 0 {
		return fmt.Errorf("invalid reported usage")
	}
	for _, finding := range report.Findings {
		if finding.Commit == "" || finding.File == "" || finding.StartLine < 1 || finding.EndLine < finding.StartLine || finding.EndLine-finding.StartLine >= 200 || strings.TrimSpace(finding.Problem) == "" || strings.TrimSpace(finding.Solution) == "" || len(finding.Evidence) == 0 || len(finding.Evidence) > 100 {
			return fmt.Errorf("finding lacks a pinned source location and evidence")
		}
	}
	return nil
}

func (e *Engine) ProcessOne(ctx context.Context, analyzer Analyzer) (bool, error) {
	if !e.Store.needsWork(e.Now(), analyzer != nil && e.Config.Budget.MaxCalls > 0) {
		return false, nil
	}
	pipeline, staged := analyzer.(PipelineAnalyzer)
	var task Investigation
	var run Run
	found := false
	err := e.update(func(s *State) error {
		found = false
		task = Investigation{}
		run = Run{}
		if err := e.recoverInvestigations(s); err != nil {
			return err
		}
		active := map[string]int{}
		for _, item := range s.Investigations {
			if activeInvestigation(item) {
				active[item.ProjectID]++
			}
		}
		for _, candidate := range fairInvestigations(*s) {
			if candidate.State != "queued" && !(candidate.State == "awaiting_configuration" && !candidate.InvocationStarted && analyzer != nil && e.Config.Budget.MaxCalls > 0) {
				continue
			}
			if active[candidate.ProjectID] >= e.Config.Worker.MaxActivePerProject || staged && routingBusy(*s, candidate.ProjectID) {
				continue
			}
			group := s.Groups[candidate.GroupID]
			if group.CurrentTaskID != candidate.ID || !taskHasActiveMembers(*s, candidate) {
				candidate.State = "superseded"
				candidate.UpdatedAt = e.Now()
				s.Investigations[candidate.ID] = candidate
				continue
			}
			task = candidate
			run = s.Runs[task.RunID]
			found = true
			if analyzer == nil || e.Config.Budget.MaxCalls == 0 {
				task.State = "awaiting_configuration"
				run.Error = "Engineering provider is not configured; no model invocation occurred."
			} else {
				task.State = "running"
				if staged {
					task.Phase = "routing"
				}
				task.Owner = e.workerID
				task.Epoch++
				task.InvocationStarted = true
				task.HeartbeatAt = e.Now()
				task.LeaseUntil = e.Now().Add(time.Duration(e.Config.Worker.LeaseSeconds) * time.Second)
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
		ticker := time.NewTicker(time.Duration(e.Config.Worker.LeaseSeconds) * time.Second / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-callCtx.Done():
				return
			case <-ticker.C:
				if err := e.update(func(s *State) error {
					current := s.Investigations[task.ID]
					if !ownsInvestigation(current, task) {
						return ErrConflict
					}
					current.HeartbeatAt = e.Now()
					current.LeaseUntil = e.Now().Add(time.Duration(e.Config.Worker.LeaseSeconds) * time.Second)
					s.Investigations[task.ID] = current
					return nil
				}); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	input := AnalyzeRequest{RunID: run.ID, Ticket: run.Input, Budget: e.Config.Budget, ReadOnly: true, Contract: &task.Contract}
	s := e.Store.Snapshot()
	records := issueRecords(s, task, run.Input)
	root := e.IssueDirectory
	var callErr error
	if root == "" {
		root, callErr = os.MkdirTemp("", "esa-issue-view-")
		if callErr == nil {
			defer os.RemoveAll(root)
		}
	}
	if callErr == nil {
		root, callErr = filepath.Abs(root)
	}
	if callErr == nil {
		input.IssueDirectory, input.IssueDirectoryHash, callErr = ExportIssueDirectory(root, run.ProjectID, task.Contract.ObservationCutoff, records)
	}
	var report Report
	invoked := callErr == nil
	if invoked {
		if staged {
			report, callErr = e.pipeline(callCtx, pipeline, input, task)
		} else {
			report, callErr = analyzer.Analyze(callCtx, input)
		}
	}
	if callErr == nil && report.Routing != nil && report.Routing.Action == "reuse" {
		directory, err := OpenIssueDirectory(root, input.IssueDirectory, input.IssueDirectoryHash)
		var record IssueRecord
		if err == nil {
			record, err = directory.ReadRecord(report.Routing.RecordPath, report.Routing.RecordHash)
		}
		if err == nil && record.Kind == "investigating" {
			err = fmt.Errorf("an investigation is not a reusable solution")
		}
		if err != nil {
			callErr = err
		} else {
			usage, alerts, accesses := report.Usage, report.Alerts, report.AccessedIssueIDs
			report = ReusedIssueReport(record, *report.Routing)
			report.Usage, report.Alerts = usage, alerts
			report.AccessedIssueIDs = accesses
		}
	}
	if callCtx.Err() != nil && callErr == nil {
		callErr = callCtx.Err()
	}
	close(done)
	<-heartbeatDone
	if callErr == nil {
		callErr = validateReport(report, input, records)
	}
	report.StoredFields = nil
	report.Envelope = reportEnvelope(task, e.Now(), time.Since(started).Milliseconds())
	report.Envelope.ProviderInputHash = hash(input)
	if invoked && report.Usage.Calls < 1 {
		report.Usage.Calls = 1
	}
	UpdateTokenAlerts(&report, e.Config.Budget)
	if !report.Usage.TokensReported {
		report.Limitations = append(report.Limitations, "Provider usage is unknown; it must not be interpreted as zero.")
	}
	err = e.update(func(s *State) error {
		current := s.Investigations[task.ID]
		if !ownsInvestigation(current, task) {
			return fmt.Errorf("%w: stale investigation result rejected", ErrConflict)
		}
		r := s.Runs[run.ID]
		// Failed or superseded work still has a cost and a diagnostic audit.
		r.Report = report
		if report.Routing != nil && report.Routing.Action == "join" {
			for _, record := range records {
				if record.Kind == "investigating" && recordPath(record) == report.Routing.RecordPath && issueRecordHash(record) == report.Routing.RecordHash {
					r.SourceTicketID = record.SourceTicketID
				}
			}
		}
		if callErr == nil {
			for _, id := range report.AccessedIssueIDs {
				touchIssue(s, id, e.Now())
			}
		}
		if !taskHasActiveMembers(*s, current) || s.Groups[current.GroupID].CurrentTaskID != current.ID {
			current.State = "superseded"
		} else if callErr != nil {
			current.State = "failed"
			r.Error = callErr.Error()
		} else if report.Routing != nil && report.Routing.Action == "return_to_support" {
			current.State, r.Route, r.Error = "returned_to_support", "customer_support", ""
		} else if report.Routing != nil && report.Routing.Action == "join" {
			if err := e.joinInvestigation(s, &current, report, records); err != nil {
				current.State, r.Error = "failed", err.Error()
			}
		} else {
			current.State = "agent_proposed"
			r.Error = ""
			if len(report.Findings) == 0 && len(report.Evidence) == 0 {
				current.State = "awaiting_information"
			}
			if report.AssignmentRouting != nil && report.AssignmentRouting.Action == "request_information" {
				current.State = "awaiting_information"
			}
			if current.State == "agent_proposed" {
				if err := e.applyIssueAssignment(s, &current, report, records); err != nil {
					current.State, r.Error = "failed", err.Error()
				}
			}
			r.IssueID = current.IssueID
			r.Report.Envelope.IssueID = current.IssueID
			if current.State == "agent_proposed" {
				group := s.Groups[current.GroupID]
				sourceID := run.TicketID
				if report.AssignmentRouting != nil && report.AssignmentRouting.Action == "assign" {
					for _, record := range records {
						if recordPath(record) == report.AssignmentRouting.RecordPath {
							r.SourceTicketID = record.SourceTicketID
						}
					}
				}
				if report.Routing != nil && report.Routing.Action == "reuse" {
					r.Route = "issue_reuse"
					for _, record := range records {
						if recordPath(record) == report.Routing.RecordPath {
							sourceID, r.SourceTicketID = record.SourceTicketID, record.SourceTicketID
						}
					}
				}

				if report.Routing != nil && report.Routing.Action == "reuse" {
					for _, record := range records {
						if recordPath(record) == report.Routing.RecordPath {
							current.Application = &SolutionApplication{SolutionID: record.SolutionID, CaseID: record.CaseID, CaseVersion: record.CaseVersion, SourceTicketID: record.SourceTicketID, State: "proposed", Revision: 1}
							// Standalone Case association may have advanced its metadata revision.
							if record.CaseID != "" {
								current.Application.CaseVersion = s.Cases[record.CaseID].Version
							}
						}
					}
					current.State = "reuse_proposed"
				} else {
					solution := SolutionRevision{ID: randomID("solution_"), IssueID: current.IssueID, InvestigationID: current.ID, ProjectID: current.ProjectID, ScopeHash: group.ScopeHash, SourceTicketID: sourceID, Origin: "agent", State: "proposed", Revision: 1, RunID: run.ID, AvailableAt: e.Now(), RecordedAt: time.Now().UTC()}
					s.Solutions[solution.ID] = solution
					current.SolutionID = solution.ID
				}

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
