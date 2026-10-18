package esa

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (e *Engine) recoverInvestigations(s *State) error {
	for _, task := range s.investigationsIn(InvestigationRunning, InvestigationInvestigating) {
		if !parked(task) && !task.LeaseUntil.After(e.Now()) {
			task.State = InvestigationFailed
			task.Epoch++
			task.UpdatedAt = e.Now()
			s.Investigations.Put(task.ID, task)
			r := s.Runs.Get(task.RunID)
			r.Error = "Investigation lease expired. Inspect the invocation audit before an explicit retry; spend is uncertain."
			r.UpdatedAt = e.Now()
			s.Runs.Put(r.ID, r)
		}
	}
	for _, r := range s.queuedRuns() {
		if r.WorkflowVersion != WorkflowVersion || r.Input.ForwardedBy == "" || r.Input.ForwardedAt.IsZero() {
			r.State = RunAwaitingForward
			r.Route = "manual_forward"
			s.Runs.Put(r.ID, r)
			if task, ok := s.Investigations.Lookup(r.InvestigationID); ok {
				task.State = InvestigationAwaitingForward
				s.Investigations.Put(task.ID, task)
			}
		}
	}
	return nil
}

// settleParked supersedes parked tasks whose members left, and fails those that
// waited longer than max_queue_wait_seconds with exact routing-only usage.
func (e *Engine) settleParked(s *State) {
	limit := time.Duration(e.Config.Worker.MaxQueueWaitSeconds) * time.Second
	for _, task := range s.investigationsIn(InvestigationInvestigating) {
		if !parked(task) {
			continue
		}
		superseded := !taskHasActiveMembers(*s, task) || s.Groups.Get(task.GroupID).CurrentTaskID != task.ID
		if !superseded && time.Since(task.WaitingSince) < limit {
			continue
		}
		run := s.Runs.Get(task.RunID)
		var input AnalyzeRequest
		if run.PipelineInput != nil {
			input = *run.PipelineInput
		}
		elapsed := time.Since(task.WaitingSince.Add(-time.Duration(task.ElapsedMS) * time.Millisecond)).Milliseconds()
		e.sealReport(&run.Report, task, input, elapsed)
		task.State = InvestigationSuperseded
		if !superseded {
			task.State, run.Error = InvestigationFailed, queueTimeout(e.Config.Worker.MaxQueueWaitSeconds).Error()
		}
		task.ElapsedMS, task.UpdatedAt = elapsed, e.Now()
		run.PipelineInput, run.UpdatedAt = nil, e.Now()
		s.Investigations.Put(task.ID, task)
		s.Runs.Put(run.ID, run)
	}
}

// sealReport adds the audit envelope and refreshes usage warnings.
func (e *Engine) sealReport(report *Report, task Investigation, input AnalyzeRequest, elapsed int64) {
	report.StoredFields = nil
	report.Envelope = reportEnvelope(task, e.Now(), elapsed)
	report.Envelope.ProviderInputHash = hash(input)
	UpdateTokenAlerts(report, e.Config.Budget)
	if !report.Usage.TokensReported {
		report.Limitations = append(report.Limitations, "Provider usage is unknown; it must not be interpreted as zero.")
	}
}

// frozenIssueRecords reloads the records a parked task was routed against from
// its content-addressed view, instead of recomputing them from current state.
func frozenIssueRecords(root string, input AnalyzeRequest) ([]IssueRecord, error) {
	view, err := OpenIssueDirectory(root, input.IssueDirectory, input.IssueDirectoryHash)
	if err != nil {
		return nil, fmt.Errorf("frozen Issue directory is unavailable: %w", err)
	}
	if input.Contract == nil || view.Manifest.ProjectID != input.Ticket.ProjectID || view.Manifest.Cutoff.After(input.Contract.ObservationCutoff) {
		return nil, errors.New("frozen Issue directory project/cutoff mismatch")
	}
	var names []string
	for name := range view.Manifest.Files {
		if strings.HasSuffix(name, "/issue.json") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	records := make([]IssueRecord, 0, len(names))
	for _, name := range names {
		record, err := view.ReadRecord(name, "")
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// issueRoot is the configured Issue projection root. Without one, a single
// process-lifetime directory keeps views readable for parked tasks.
func (e *Engine) issueRoot() (string, error) {
	if e.IssueDirectory != "" {
		return filepath.Abs(e.IssueDirectory)
	}
	e.tempRootOnce.Do(func() { e.tempRoot, e.tempRootErr = os.MkdirTemp("", "esa-issue-view-") })
	return e.tempRoot, e.tempRootErr
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

// ProcessOne claims at most one eligible shared task and executes its provider stages.
// When an engineering slot is free it first resumes the oldest parked task,
// rotating across projects; otherwise it claims a queued ticket for routing.
// It maintains a lease heartbeat and rejects results from superseded owners/epochs.
// Returns whether work was selected plus an error; no configured provider means no model call.
// Usage and alerts remain auditable even when report validation fails.
func (e *Engine) ProcessOne(ctx context.Context, analyzer Analyzer) (bool, error) {
	if !e.Store.needsWork(e.Now(), analyzer != nil && e.Config.Budget.MaxCalls > 0, time.Duration(e.Config.Worker.MaxQueueWaitSeconds)*time.Second) {
		return false, nil
	}
	pipeline, staged := analyzer.(PipelineAnalyzer)
	var task Investigation
	var run Run
	found, resumed := false, false
	err := e.update(func(s *State) error {
		found, resumed = false, false
		task = Investigation{}
		run = Run{}
		if err := e.recoverInvestigations(s); err != nil {
			return err
		}
		e.settleParked(s)
		// Parked tasks hold no worker, so they do not count toward the project limit.
		active := map[string]int{}
		for _, item := range s.investigationsIn(InvestigationRunning, InvestigationInvestigating) {
			if !parked(item) {
				active[item.ProjectID]++
			}
		}
		if staged && e.Config.Budget.MaxCalls > 0 && engineeringActive(*s) < EngineeringConcurrency {
			for _, candidate := range fairParked(*s) {
				if active[candidate.ProjectID] >= e.Config.Worker.MaxActivePerProject {
					continue
				}
				// Claiming reserves the slot; the new epoch fences any earlier owner.
				candidate.Phase, candidate.Owner = "investigating", e.workerID
				candidate.Epoch++
				candidate.HeartbeatAt = e.Now()
				candidate.LeaseUntil = e.Now().Add(time.Duration(e.Config.Worker.LeaseSeconds) * time.Second)
				candidate.UpdatedAt = e.Now()
				s.Investigations.Put(candidate.ID, candidate)
				s.LastClaimedProject = candidate.ProjectID
				task, run = candidate, s.Runs.Get(candidate.RunID)
				found, resumed = true, true
				return nil
			}
		}
		claimable := []InvestigationState{InvestigationQueued}
		if analyzer != nil && e.Config.Budget.MaxCalls > 0 {
			claimable = append(claimable, InvestigationAwaitingConfiguration)
		}
		for _, candidate := range fairInvestigations(*s, claimable...) {
			if candidate.State == InvestigationAwaitingConfiguration && candidate.InvocationStarted {
				continue
			}
			if active[candidate.ProjectID] >= e.Config.Worker.MaxActivePerProject || staged && routingBusy(*s, candidate.ProjectID) {
				continue
			}
			group := s.Groups.Get(candidate.GroupID)
			if group.CurrentTaskID != candidate.ID || !taskHasActiveMembers(*s, candidate) {
				candidate.State = InvestigationSuperseded
				candidate.UpdatedAt = e.Now()
				s.Investigations.Put(candidate.ID, candidate)
				continue
			}
			task = candidate
			run = s.Runs.Get(task.RunID)
			found = true
			if analyzer == nil || e.Config.Budget.MaxCalls == 0 {
				task.State = InvestigationAwaitingConfiguration
				run.Error = "Engineering provider is not configured; no model invocation occurred."
			} else {
				task.State = InvestigationRunning
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
			s.Investigations.Put(task.ID, task)
			s.Runs.Put(run.ID, run)
			break
		}
		return nil
	})
	if err != nil || !found || task.State != InvestigationRunning && !resumed {
		return found, err
	}
	started := time.Now()
	if resumed {
		// Elapsed time spans routing, waiting and the resumed stages.
		started = task.WaitingSince.Add(-time.Duration(task.ElapsedMS) * time.Millisecond)
	}
	// The lease heartbeat cancels runCtx when ownership is lost. A staged pipeline
	// sets per-stage deadlines; a single-call analyzer keeps one call deadline.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	callCtx := runCtx
	if !staged {
		var cancelCall context.CancelFunc
		callCtx, cancelCall = context.WithTimeout(runCtx, time.Duration(e.Config.Budget.TimeoutSeconds)*time.Second)
		defer cancelCall()
	}
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
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := e.update(func(s *State) error {
					current := s.Investigations.Get(task.ID)
					if !ownsInvestigation(current, task) {
						return ErrConflict
					}
					current.HeartbeatAt = e.Now()
					current.LeaseUntil = e.Now().Add(time.Duration(e.Config.Worker.LeaseSeconds) * time.Second)
					s.Investigations.Put(task.ID, current)
					return nil
				}); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	input := AnalyzeRequest{RunID: run.ID, Ticket: run.Input, Budget: e.Config.Budget, ReadOnly: true, Contract: &task.Contract}
	var records []IssueRecord
	var report Report
	root, callErr := e.issueRoot()
	if resumed {
		// The provider keeps per-run state between stages, so a resumed task sends
		// the same run ID and frozen request, and validates against the same records.
		report = run.Report
		if run.PipelineInput == nil {
			callErr = errors.New("parked task has no frozen provider input")
		} else {
			input = *run.PipelineInput
		}
		if callErr == nil {
			records, callErr = frozenIssueRecords(root, input)
		}
	} else {
		records = issueRecords(e.Store.Snapshot(), task, run.Input)
		if callErr == nil {
			input.IssueDirectory, input.IssueDirectoryHash, callErr = ExportIssueDirectory(root, run.ProjectID, task.Contract.ObservationCutoff, records)
		}
	}
	invoked := callErr == nil
	if invoked {
		if resumed {
			report, callErr = e.engineer(callCtx, pipeline, input, task, report)
		} else if staged {
			report, callErr = e.pipeline(callCtx, pipeline, input, task, started)
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
	if errors.Is(callErr, errParked) {
		// The parking transaction already stored the routed report and released
		// ownership; the worker is free for other tickets.
		return true, nil
	}
	defer e.Store.signalWork()
	if callErr == nil {
		callErr = validateReport(report, input, records)
	}
	if invoked && report.Usage.Calls < 1 {
		report.Usage.Calls = 1
	}
	e.sealReport(&report, task, input, time.Since(started).Milliseconds())
	err = e.update(func(s *State) error {
		current := s.Investigations.Get(task.ID)
		if !ownsInvestigation(current, task) {
			return fmt.Errorf("%w: stale investigation result rejected", ErrConflict)
		}
		r := s.Runs.Get(run.ID)
		// Failed or superseded work still has a cost and a diagnostic audit.
		r.Report, r.PipelineInput = report, nil
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
		if !taskHasActiveMembers(*s, current) || s.Groups.Get(current.GroupID).CurrentTaskID != current.ID {
			current.State = InvestigationSuperseded
		} else if callErr != nil {
			current.State = InvestigationFailed
			r.Error = callErr.Error()
		} else if report.Routing != nil && report.Routing.Action == "return_to_support" {
			current.State, r.Route, r.Error = InvestigationReturnedToSupport, "customer_support", ""
		} else if report.Routing != nil && report.Routing.Action == "join" {
			if err := e.joinInvestigation(s, &current, report, records); err != nil {
				current.State, r.Error = InvestigationFailed, err.Error()
			}
		} else {
			current.State = InvestigationAgentProposed
			r.Error = ""
			if len(report.Findings) == 0 && len(report.Evidence) == 0 {
				current.State = InvestigationAwaitingInformation
			}
			if report.AssignmentRouting != nil && report.AssignmentRouting.Action == "request_information" {
				current.State = InvestigationAwaitingInformation
			}
			if current.State == InvestigationAgentProposed {
				if err := e.applyIssueAssignment(s, &current, report, records); err != nil {
					current.State, r.Error = InvestigationFailed, err.Error()
				}
			}
			r.IssueID = current.IssueID
			r.Report.Envelope.IssueID = current.IssueID
			if current.State == InvestigationAgentProposed {
				group := s.Groups.Get(current.GroupID)
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
							current.Application = &SolutionApplication{SolutionID: record.SolutionID, CaseID: record.CaseID, CaseVersion: record.CaseVersion, SourceTicketID: record.SourceTicketID, State: ApplicationProposed, Revision: 1}
							// Standalone Case association may have advanced its metadata revision.
							if record.CaseID != "" {
								current.Application.CaseVersion = s.Cases.Get(record.CaseID).Version
							}
						}
					}
					current.State = InvestigationReuseProposed
				} else {
					solution := SolutionRevision{ID: randomID("solution_"), IssueID: current.IssueID, InvestigationID: current.ID, ProjectID: current.ProjectID, ScopeHash: group.ScopeHash, SourceTicketID: sourceID, Origin: "agent", State: SolutionProposed, Revision: 1, RunID: run.ID, AvailableAt: e.Now(), RecordedAt: time.Now().UTC()}
					s.Solutions.Put(solution.ID, solution)
					current.SolutionID = solution.ID
				}

			}
		}
		current.ElapsedMS = time.Since(started).Milliseconds()
		current.UpdatedAt = e.Now()
		current.LeaseUntil = time.Time{}
		r.UpdatedAt = e.Now()
		s.Investigations.Put(current.ID, current)
		s.Runs.Put(r.ID, r)
		return nil
	})
	return true, err
}
