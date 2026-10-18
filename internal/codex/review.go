package codex

import (
	"context"
	"encoding/json"
	"esa/internal/codexreview"
	"esa/internal/esa"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ReviewSchema() []byte {
	assessment := enum("correct", "incorrect", "unknown")
	finding := object(map[string]any{"index": map[string]any{"type": "integer", "minimum": 0}, "localization": assessment, "problem": assessment, "solution": assessment, "explanation": str()})
	schema := object(map[string]any{"commit": str(), "verdict": enum("resolved", "not_resolved", "regressed", "uncertain"), "explanation": str(), "findings": array(finding), "evidence": array(evidenceSchema()), "limitations": array(str())})
	b, _ := json.Marshal(schema)
	return b
}

// Review starts a fresh retrospective session after the original report is frozen.
// It receives the prediction intentionally: this implements PR-based reverse
// assessment, rather than an independently constructed ground-truth dataset.
func (p *Provider) Review(ctx context.Context, request codexreview.ReviewRequest, budget esa.Budget, runID string) (codexreview.Review, error) {
	var r codexreview.Review
	if budget.MaxCalls != 1 || p.ToolsConfig != "" || !strings.HasPrefix(runID, "run_") || strings.ContainsAny(runID, "/\\.") {
		return r, fmt.Errorf("retrospective review requires a separate tool-free run")
	}
	if request.Candidate.Commit == "" || request.Baseline == "" {
		return r, fmt.Errorf("frozen prediction and candidate are required")
	}
	dir := filepath.Join(p.RunsDir, runID)
	if err := os.MkdirAll(p.RunsDir, 0700); err != nil {
		return r, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return r, fmt.Errorf("review attempt exists; inspect it before retrying: %w", err)
	}
	// Explicit numbers reduce citation errors in long PR patches.
	type numbered struct {
		Source, Prefix string
		Lines          []string
	}
	visible := struct {
		Request           codexreview.ReviewRequest
		NumberedDocuments []numbered
	}{Request: request}
	// Keep full packets on disk, but avoid sending duplicate patch, message,
	// runtime logs and derived display fields to the model.
	visible.Request.Documents = nil
	visible.Request.Candidate.Patch = ""
	visible.Request.Candidate.Message = ""
	visible.Request.Issue.Findings = nil
	visible.Request.Prediction = esa.Report{Issue: request.Prediction.Issue, Findings: request.Prediction.Findings, Limitations: request.Prediction.Limitations}
	if request.Runtime != nil {
		visible.Request.Runtime = map[string]any{}
		for phase, value := range request.Runtime {
			if observation, ok := value.(map[string]any); ok {
				compact := map[string]any{}
				for _, key := range []string{"commit", "exit_code", "runtime_image", "profile", "scope", "state", "log_sha256"} {
					if v, ok := observation[key]; ok {
						compact[key] = v
					}
				}
				visible.Request.Runtime[phase] = compact
			} else {
				visible.Request.Runtime[phase] = value
			}
		}
	}
	for _, d := range request.Documents {
		n := numbered{Source: d.Source, Prefix: d.Prefix}
		for i, line := range d.Lines {
			n.Lines = append(n.Lines, fmt.Sprintf("%d: %s", d.First+i, line))
		}
		visible.NumberedDocuments = append(visible.NumberedDocuments, n)
	}
	b, _ := json.Marshal(visible)
	instructions := reviewInstructions
	prompt := reviewPrompt + "\n" + string(b)
	for name, data := range map[string][]byte{"input.json": mustJSON(request), "schema.json": ReviewSchema(), "instructions.txt": []byte(instructions), "prompt.txt": []byte(prompt)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return r, err
		}
	}
	answer, usage, alerts, err := p.invoke(ctx, dir, prompt, request.SourceKey, budget)
	r.Usage = usage
	r.Alerts = alerts
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(answer, &r); err != nil {
		return r, err
	}
	r.Usage = usage
	r.Alerts = alerts
	if err = codexreview.ValidateReview(r, request); err != nil {
		return r, err
	}
	if err = os.WriteFile(filepath.Join(dir, "review.json"), mustJSON(r), 0600); err != nil {
		return r, err
	}
	return r, nil
}
func mustJSON(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return b }
