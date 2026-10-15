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

// AnalyzeRequest carries frozen input, budget, contract and an Issue-directory path/hash.
// It deliberately carries no inline archive or caller-selected Issue assignment.
type AnalyzeRequest struct {
	Stage              string        `json:"stage,omitempty"`
	Contract           *TaskContract `json:"task_contract,omitempty"`
	IssueDirectory     string        `json:"issue_directory,omitempty"`
	IssueDirectoryHash string        `json:"issue_directory_hash,omitempty"`
	RunID              string        `json:"run_id"`
	Ticket             Ticket        `json:"ticket"`
	Budget             Budget        `json:"budget"`
	ReadOnly           bool          `json:"read_only"`
}

// Analyzer accepts a read-only contract and returns a report with measured usage.
// Implementations must retain incurred usage even when returning an error.
type Analyzer interface {
	Analyze(context.Context, AnalyzeRequest) (Report, error)
}

// PipelineAnalyzer separates intake routing from the expensive diagnostic turn.
// ESA commits the in-progress claim before dispatching the continuation.
type PipelineAnalyzer interface {
	Analyzer
	AnalyzeStage(context.Context, AnalyzeRequest, string) (Report, error)
}

type HTTPPipelineAnalyzer struct{ HTTPAnalyzer }

func (a HTTPPipelineAnalyzer) AnalyzeStage(ctx context.Context, input AnalyzeRequest, stage string) (Report, error) {
	input.Stage = stage
	return a.Analyze(ctx, input)
}

// HTTPAnalyzer calls an operator-configured provider; it cannot sandbox a remote service.
// Providers enforce source immutability; token thresholds remain advisory.
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
	return e.update(e.recoverInvestigations)
}
