package esa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

type IssueBrief struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	TicketKeys []string `json:"ticket_keys"`
}
type AnalyzeRequest struct {
	Contract    *TaskContract `json:"task_contract,omitempty"`
	KnownIssues []IssueBrief  `json:"known_issues,omitempty"`
	RunID       string        `json:"run_id"`
	Ticket      Ticket        `json:"ticket"`
	Cases       []Case        `json:"cases"`
	Budget      Budget        `json:"budget"`
	ReadOnly    bool          `json:"read_only"`
}

type Analyzer interface {
	Analyze(context.Context, AnalyzeRequest) (Report, error)
}

// HTTPAnalyzer delegates engineering analysis to a configured read-only provider.
// Providers enforce source immutability; token thresholds generate warnings.
type HTTPAnalyzer struct {
	URL, Token string
	Client     *http.Client
}

func (a HTTPAnalyzer) Analyze(ctx context.Context, input AnalyzeRequest) (Report, error) {
	var report Report
	body, err := json.Marshal(input)
	if err != nil {
		return report, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
	if err != nil {
		return report, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: time.Duration(input.Budget.TimeoutSeconds) * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return report, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return report, fmt.Errorf("engineering provider returned HTTP %d", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&report); err != nil {
		return report, err
	}
	return report, nil
}

func (e *Engine) Recover() error {
	return e.Store.Update(func(s *State) error {
		if e.Config.V2.Enabled {
			return e.recoverShared(s)
		}
		for id, r := range s.Runs {
			if r.State == "running" {
				r.State = "failed"
				r.Error = "Execution was interrupted. Review provider activity before retrying to avoid duplicate model calls."
				r.UpdatedAt = e.Now()
				s.Runs[id] = r
			}
		}
		return nil
	})
}

// ProcessOne claims one queued task. Network calls never hold the storage lock.
func (e *Engine) ProcessOne(ctx context.Context, analyzer Analyzer) (bool, error) {
	if e.Config.V2.Enabled {
		return e.processSharedOne(ctx, analyzer)
	}
	if !e.Store.HasQueuedRun() {
		return false, nil
	}
	var run Run
	found := false
	err := e.Store.Update(func(s *State) error {
		found = false
		run = Run{}
		ids := []string{}
		for id, r := range s.Runs {
			if r.State == "queued" {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool {
			a, b := s.Runs[ids[i]], s.Runs[ids[j]]
			if a.CreatedAt.Equal(b.CreatedAt) {
				return a.ID < b.ID
			}
			return a.CreatedAt.Before(b.CreatedAt)
		})
		for _, id := range ids {
			r := s.Runs[id]
			if s.Latest[r.TicketID] != id || s.Tickets[r.TicketID].Closed {
				r.State = "superseded"
				r.UpdatedAt = e.Now()
				s.Runs[id] = r
				continue
			}
			if r.WorkflowVersion != WorkflowVersion || r.Input.ForwardedBy == "" || r.Input.ForwardedAt.IsZero() || r.Input.ForwardedAt.After(e.Now()) {
				r.State = "awaiting_forward"
				r.Route = "manual_forward"
				r.MissingInformation = []string{"Forward the current ticket manually before ESA processing."}
				r.UpdatedAt = e.Now()
				s.Runs[id] = r
				continue
			}
			found = true
			r.UpdatedAt = e.Now()
			if analyzer == nil || e.Config.Budget.MaxCalls == 0 {
				r.State = "awaiting_configuration"
				r.Error = "Engineering analysis is not configured. No model call was made."
			} else {
				r.State = "running"
				r.Report.Usage.Calls = 1
			}
			s.Runs[id] = r
			run = r
			break
		}
		return nil
	})
	if err != nil || !found || run.State != "running" {
		return found, err
	}
	state := e.Store.Snapshot()
	matched := []Case{}
	for _, m := range run.Matches {
		for _, c := range state.CaseHistory {
			if c.ID == m.CaseID && c.Version == m.CaseVersion {
				matched = append(matched, c)
				break
			}
		}
	}
	input := AnalyzeRequest{RunID: run.ID, Ticket: run.Input, Cases: matched, Budget: e.Config.Budget, ReadOnly: true}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(e.Config.Budget.TimeoutSeconds)*time.Second)
	report, callErr := analyzer.Analyze(callCtx, input)
	cancel()
	if callErr == nil && (report.Summary == "" || (len(report.Evidence) == 0 && (len(report.Findings) > 0 || len(report.Limitations) == 0))) {
		callErr = errors.New("engineering provider did not return a summary and evidence")
	}
	if callErr == nil {
		for _, ev := range report.Evidence {
			if ev.Source == "" || ev.Reference == "" || ev.Quote == "" {
				callErr = errors.New("engineering evidence is missing its source, reference, or quote")
				break
			}
		}
	}
	report.AgentClassification = nil // Classification output is no longer an active contract.
	if report.Usage.InputTokens < 0 || report.Usage.OutputTokens < 0 || report.Usage.Calls > e.Config.Budget.MaxCalls {
		callErr = errors.New("engineering provider reported invalid usage or exceeded the invocation limit")
	}
	report.Usage.Calls = 1
	AddAlerts(&report, TokenAlerts(report.Usage, e.Config.Budget)...)
	if !report.Usage.TokensReported {
		report.Limitations = append(report.Limitations, "The provider did not report token usage; usage must not be interpreted as zero.")
	}
	err = e.Store.Update(func(s *State) error {
		r := s.Runs[run.ID]
		if r.State == "superseded" {
			return nil
		}
		r.Report = report
		r.UpdatedAt = e.Now()
		if s.Latest[r.TicketID] != r.ID || s.Tickets[r.TicketID].Closed {
			r.State = "superseded"
		} else if callErr != nil {
			r.State = "failed"
			r.Error = callErr.Error()
		} else {
			r.State = "awaiting_review"
			if len(report.Findings) == 0 && len(report.Evidence) == 0 {
				r.State = "awaiting_information"
			}
			r.Error = ""
		}
		s.Runs[r.ID] = r
		return nil
	})
	return true, err
}

func (e *Engine) ReviewRun(id string, review Review) (Run, error) {
	var result Run
	err := e.Store.Update(func(s *State) error {
		r, ok := s.Runs[id]
		if !ok {
			return ErrNotFound
		}
		if r.InvestigationID != "" {
			return fmt.Errorf("%w: review the versioned investigation solution", ErrInvalid)
		}
		if r.State != "awaiting_review" || review.Reviewer == "" || !contains([]string{"approve", "reject"}, review.Action) {
			return fmt.Errorf("%w: a reviewable run, reviewer, and approve/reject action are required", ErrInvalid)
		}
		if s.Latest[r.TicketID] != id {
			return fmt.Errorf("%w: a newer analysis exists", ErrConflict)
		}
		if review.Action == "approve" {
			r.State = "completed"
		} else {
			r.State = "rejected"
		}
		r.Report.Limitations = append(r.Report.Limitations, "Reviewed by "+review.Reviewer+": "+review.Note)
		r.UpdatedAt = e.Now()
		s.Runs[id] = r
		result = r
		return nil
	})
	return result, err
}
