package esa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		var failure struct {
			Report Report `json:"report"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&failure) == nil {
			report = failure.Report
		}
		return report, fmt.Errorf("engineering provider returned HTTP %d", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&report); err != nil {
		return report, err
	}
	return report, nil
}

func (e *Engine) Recover() error {
	return e.Store.Update(e.recoverInvestigations)
}
