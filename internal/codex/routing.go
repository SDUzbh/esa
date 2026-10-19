package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"esa/internal/esa"
	"esa/internal/replaytools"
)

// Analyze dispatches the read-only workflow, preserving stage audits and usage.
// Reuse and support returns skip engineering; new findings return to Routing for assignment.
// Standalone evidence trials without a Task Contract exercise engineering only.
func (p *Provider) Analyze(ctx context.Context, in esa.AnalyzeRequest) (esa.Report, error) {
	if in.Stage != "" && in.Contract == nil {
		return esa.Report{}, errors.New("pipeline stages require a Task Contract")
	}
	p.slotsOnce.Do(func() {
		if p.Slots == nil {
			p.Slots = make(chan struct{}, 3)
		}
	})
	if cap(p.Slots) < 1 || cap(p.Slots) > 3 {
		return esa.Report{}, errors.New("engineering concurrency must be 1..3")
	}
	if in.Contract == nil {
		return p.analyzeEngineering(ctx, in)
	}
	if !in.ReadOnly ||
		!in.Contract.ReadOnly ||
		in.Ticket.Private ||
		in.Ticket.Closed ||
		in.Budget.MaxCalls != 1 ||
		in.Contract.ObservationCutoff.IsZero() ||
		!validRunID(in.RunID) {
		return esa.Report{}, errors.New("invalid routing request")
	}
	stage := in.Stage
	if stage != "" && stage != "route" && stage != "engineer" && stage != "assign" {
		return esa.Report{}, errors.New("unsupported pipeline stage")
	}
	in.Stage = ""
	if _, busy := p.workflows.LoadOrStore(in.RunID, struct{}{}); busy {
		return esa.Report{}, errors.New("run is already executing")
	}
	defer p.workflows.Delete(in.RunID)
	directory, err := esa.OpenIssueDirectory(p.IssuesRoot, in.IssueDirectory, in.IssueDirectoryHash)
	if err != nil {
		return esa.Report{}, err
	}
	if directory.Manifest.ProjectID != in.Ticket.ProjectID || directory.Manifest.Cutoff.After(in.Contract.ObservationCutoff) {
		return esa.Report{}, errors.New("issue directory project/cutoff mismatch")
	}
	dir := filepath.Join(p.RunsDir, in.RunID)
	raw, _ := json.Marshal(in)
	if old, err := os.ReadFile(filepath.Join(dir, "input.json")); err == nil {
		if !bytes.Equal(raw, old) {
			return esa.Report{}, errors.New("run ID reused with different input")
		}
		if stage == "engineer" || stage == "assign" {
			file := "engineering-report.json"
			if stage == "assign" {
				file = "report.json"
			}
			if data, err := os.ReadFile(filepath.Join(dir, file)); err == nil {
				var cached esa.Report
				err = json.Unmarshal(data, &cached)
				return cached, err
			}
			previous := "routing-report.json"
			if stage == "assign" {
				previous = "engineering-report.json"
			}
			data, err := os.ReadFile(filepath.Join(dir, previous))
			if err != nil {
				return esa.Report{}, errors.New("previous pipeline stage has not completed")
			}
			var report esa.Report
			if err = json.Unmarshal(data, &report); err != nil {
				return report, err
			}
			if report.Routing == nil || report.Routing.Action != "analyze" {
				return report, errors.New("only analyze requests can continue")
			}
			if stage == "engineer" {
				return p.engineerAnalysis(ctx, in, dir, report)
			}
			return p.assignAnalysis(ctx, in, dir, directory, report)
		}
		file := "report.json"
		if stage == "route" {
			file = "routing-report.json"
		}
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return esa.Report{}, errors.New("previous routing attempt exists; use an explicit new run")
		}
		var report esa.Report
		err = json.Unmarshal(data, &report)
		return report, err
	}
	if stage == "engineer" || stage == "assign" {
		return esa.Report{}, errors.New("no intake routing audit exists")
	}
	if err = os.MkdirAll(p.RunsDir, 0700); err != nil {
		return esa.Report{}, err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return esa.Report{}, errors.New("previous routing attempt exists; inspect the invocation audit")
	}
	if err = os.WriteFile(filepath.Join(dir, "input.json"), raw, 0600); err != nil {
		return esa.Report{}, err
	}
	routingDir := filepath.Join(dir, "routing")
	payload := struct {
		Ticket    esa.Ticket `json:"ticket"`
		Directory string     `json:"issue_directory"`
		Cutoff    any        `json:"observation_cutoff"`
	}{in.Ticket, filepath.Join(p.IssuesRoot, filepath.FromSlash(in.IssueDirectory)), in.Contract.ObservationCutoff}
	promptData, _ := json.Marshal(payload)
	prompt := routingPrompt + "\n" + string(promptData)
	instructions := routingInstructions
	schema := object(map[string]any{
		"action":      enum("reuse", "join", "analyze", "request_information", "return_to_support"),
		"record_path": str(),
		"record_hash": str(),
		"reason":      str(),
	})
	routing, alerts, err := p.routingStage(ctx, in, routingDir, "", prompt, instructions, schema)
	result := esa.Report{Usage: routing.Usage, Alerts: alerts, Routing: &routing}
	if err != nil {
		return result, err
	}
	searched, read := routingAudit(filepath.Join(routingDir, "tools.jsonl"), routing.RecordPath, routing.RecordHash)
	if !searched && routing.Action != "return_to_support" {
		return result, errors.New("Routing Agent must search the registered issue directory")
	}
	switch routing.Action {
	case "reuse":
		if !read {
			return result, errors.New("Routing Agent must read the exact record before reuse")
		}
		record, err := directory.ReadRecord(routing.RecordPath, routing.RecordHash)
		if err != nil {
			return result, err
		}
		if record.Kind == "investigating" {
			return result, errors.New("an investigation is not a reusable solution")
		}
		result = esa.ReusedIssueReport(record, routing)
		result.Alerts = alerts
	case "return_to_support":
		if routing.RecordPath != "" || routing.RecordHash != "" {
			return result, errors.New("customer support routing cannot select a solution")
		}
		result.Summary = "Non-engineering request returned to customer support"
		result.Recommendations = []string{routing.Reason}
		result.Limitations = []string{"No engineering diagnosis was performed. Continue through the normal customer support process."}
	case "request_information":
		if routing.RecordPath != "" || routing.RecordHash != "" {
			return result, errors.New("clarification cannot select a solution")
		}
		result.Summary = "Additional ticket information required"
		result.Limitations = []string{routing.Reason}
	case "analyze":
		if routing.RecordPath != "" || routing.RecordHash != "" {
			return result, errors.New("new analysis cannot claim a reused solution")
		}
		// Gate the handoff before claiming work or invoking engineering. The raw
		// model decision remains in routing/answer.json for the invocation audit.
		reason := requiredRelease(in.Ticket)
		if reason == "" && p.CheckRelease != nil {
			reason = p.CheckRelease(engineeringInput(in))
		}
		if reason != "" {
			routing.Action, routing.Reason = "request_information", reason
			result.Summary = "Additional ticket information required"
			result.Limitations = []string{reason}
			break
		}
		if stage == "route" {
			result.Summary = "Ticket routed for engineering analysis"
			result.Limitations = []string{"Engineering analysis has not started."}
			result.AccessedIssueIDs = readIssueAccesses(directory, filepath.Join(routingDir, "tools.jsonl"))
			data, _ := json.MarshalIndent(result, "", "  ")
			err = os.WriteFile(filepath.Join(dir, "routing-report.json"), data, 0600)
			return result, err
		}
		report, err := p.engineerAnalysis(ctx, in, dir, result)
		if err != nil {
			return report, err
		}
		return p.assignAnalysis(ctx, in, dir, directory, report)
	case "join":
		if stage != "route" || !read {
			return result, errors.New("joining requires pipeline routing and an exact audited record read")
		}
		record, err := directory.ReadRecord(routing.RecordPath, routing.RecordHash)
		if err != nil || record.Kind != "investigating" || record.InvestigationID == "" {
			return result, errors.New("join requires an investigating record")
		}
		result.Summary = "Joined an existing investigation"
		result.Limitations = []string{"An investigation is in progress; no solution or new engineering work is claimed."}
	default:
		return result, errors.New("unsupported routing action")
	}
	esa.UpdateTokenAlerts(&result, in.Budget)
	result.AccessedIssueIDs = mergeIssueAccesses(readIssueAccesses(directory, filepath.Join(routingDir, "tools.jsonl")), result.AccessedIssueIDs)
	data, _ := json.MarshalIndent(result, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "report.json"), data, 0600); err != nil {
		return result, err
	}
	if stage == "route" {
		err = os.WriteFile(filepath.Join(dir, "routing-report.json"), data, 0600)
	}
	return result, err
}

func (p *Provider) engineerAnalysis(ctx context.Context, in esa.AnalyzeRequest, dir string, routed esa.Report) (esa.Report, error) {
	if err := os.Mkdir(filepath.Join(dir, "engineering-attempt"), 0700); err != nil {
		return esa.Report{}, errors.New("engineering already attempted; inspect the audit before an explicit new run")
	}
	engineer := p.child()
	engineer.RunsDir = filepath.Join(dir, "engineering")
	report, err := engineer.analyzeEngineering(ctx, in)
	report.Routing = routed.Routing
	report.Usage = combineUsage(routed.Usage, report.Usage)
	report.AccessedIssueIDs = routed.AccessedIssueIDs
	esa.AddAlerts(&report, routed.Alerts...)
	esa.UpdateTokenAlerts(&report, in.Budget)
	if err != nil {
		return report, err
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	err = os.WriteFile(filepath.Join(dir, "engineering-report.json"), data, 0600)
	return report, err
}

func (p *Provider) assignAnalysis(ctx context.Context, in esa.AnalyzeRequest, dir string, directory esa.IssueDirectory, report esa.Report) (esa.Report, error) {
	if err := os.Mkdir(filepath.Join(dir, "assignment-attempt"), 0700); err != nil {
		return esa.Report{}, errors.New("assignment already attempted; inspect the audit before an explicit new run")
	}
	routingDir := filepath.Join(dir, "routing")
	result := report
	if len(report.Findings) > 0 || len(report.Evidence) > 0 {
		decision, issue, alerts, err := p.assignIssue(ctx, in, dir, routingDir, report)
		result.AssignmentRouting, result.Issue = &decision, issue
		result.Usage = combineUsage(result.Usage, decision.Usage)
		esa.AddAlerts(&result, alerts...)
		result.AccessedIssueIDs = mergeIssueAccesses(result.AccessedIssueIDs, readIssueAccesses(directory, filepath.Join(dir, "assignment", "tools.jsonl")))
		if err != nil {
			return result, err
		}
		if decision.Action == "request_information" {
			result.Limitations = append(result.Limitations, decision.Reason)
		}
	}
	esa.UpdateTokenAlerts(&result, in.Budget)
	data, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0600); err != nil {
		return result, err
	}
	return result, nil
}
func validRunID(id string) bool {
	return strings.HasPrefix(id, "run_") && !strings.ContainsAny(id, "/\\.")
}
func (p *Provider) child() *Provider {
	return &Provider{
		CLI:               p.CLI,
		Model:             p.Model,
		Effort:            p.Effort,
		RunsDir:           p.RunsDir,
		ToolsConfig:       p.ToolsConfig,
		ToolCommand:       p.ToolCommand,
		ProfilesRoot:      p.ProfilesRoot,
		CheckRelease:      p.CheckRelease,
		Resolve:           p.Resolve,
		ToolCatalog:       p.ToolCatalog,
		MavenRepository:   p.MavenRepository,
		MavenBeforeCutoff: p.MavenBeforeCutoff,
		SetupEnvironment:  p.SetupEnvironment,
		Packet:            p.Packet,
		IssuesRoot:        p.IssuesRoot,
		Slots:             p.Slots,
	}
}
func combineUsage(a, b esa.Usage) esa.Usage {
	return esa.Usage{
		Calls:             a.Calls + b.Calls,
		InputTokens:       a.InputTokens + b.InputTokens,
		CachedInputTokens: a.CachedInputTokens + b.CachedInputTokens,
		OutputTokens:      a.OutputTokens + b.OutputTokens,
		TokensReported:    (a.Calls == 0 || a.TokensReported) && (b.Calls == 0 || b.TokensReported),
	}
}
func routingAudit(file, selected, digest string) (searched, read bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var entry replaytools.Record
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if entry.Tool == "issue_search" {
			searched = true
		}
		if entry.Tool == "issue_read" {
			var args struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
			}
			if json.Unmarshal(entry.Arguments, &args) == nil && args.Path == selected && args.SHA256 == digest {
				read = true
			}
		}
	}
	return
}

// Only successful, hash-verified record reads count as catalogue accesses.
// Search results and index export never refresh every candidate's LRU order.
func readIssueAccesses(directory esa.IssueDirectory, audit string) []string {
	data, err := os.ReadFile(audit)
	if err != nil {
		return nil
	}
	ids := []string{}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var entry replaytools.Record
		var args struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Tool != "issue_read" || json.Unmarshal(entry.Arguments, &args) != nil || args.SHA256 == "" {
			continue
		}
		if record, err := directory.ReadRecord(args.Path, args.SHA256); err == nil && record.IssueID != "" {
			ids = append(ids, record.IssueID)
		}
	}
	return mergeIssueAccesses(ids, nil)
}
func mergeIssueAccesses(a, b []string) []string {
	set := map[string]bool{}
	all := append(append([]string{}, a...), b...)
	ids := make([]string, 0, len(set))
	for i := len(all) - 1; i >= 0; i-- {
		if !set[all[i]] {
			set[all[i]] = true
			ids = append(ids, all[i])
		}
	}
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return ids
}
