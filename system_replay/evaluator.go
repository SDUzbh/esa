package systemreplay

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// An evaluator is a separate external service. It cannot override the runtime
// failure/pass gate and receives only already-frozen public-system output.
func evaluateRepair(ctx context.Context, m Manifest, result Result, repair Repair) (any, error) {
	if m.Evaluator == nil || repair.State != "behavior_verified" {
		return nil, nil
	}
	if len(result.Events) == 0 {
		return nil, fmt.Errorf("no public prediction to evaluate")
	}
	prediction := initialObservation(result)
	type document struct {
		Source string   `json:"source"`
		Prefix string   `json:"reference_prefix"`
		First  int      `json:"first_line"`
		Lines  []string `json:"lines"`
	}
	documents := []document{{"patch", repair.Candidate.Commit + ":patch:", 1, strings.Split(repair.Candidate.Patch, "\n")}, {"git", repair.Candidate.Commit + ":message:", 1, strings.Split(repair.Candidate.Message, "\n")}}
	for _, record := range []RuntimeRecord{repair.Before, repair.After} {
		if log, ok := record.Outcome["log"].(string); ok {
			documents = append(documents, document{"replay", record.Commit + ":log:", 1, strings.Split(log, "\n")})
		}
	}
	for _, finding := range prediction.Findings {
		if finding.Commit != result.Plan.Commit {
			return nil, fmt.Errorf("observed finding is not at the historical source commit")
		}
		if finding.StartLine < 1 || finding.EndLine < finding.StartLine {
			return nil, fmt.Errorf("observed finding range is invalid")
		}
		code, e := (Git{m.Repo, ctx}).run("show", finding.Commit+":"+finding.File)
		if e != nil {
			return nil, e
		}
		lines := strings.Split(code, "\n")
		first, last := finding.StartLine-5, finding.EndLine+5
		if first < 1 {
			first = 1
		}
		if last > len(lines) {
			last = len(lines)
		}
		if first > last {
			return nil, fmt.Errorf("observed finding line range is invalid")
		}
		documents = append(documents, document{"git", finding.Commit + ":" + finding.File + ":", first, lines[first-1 : last]})
	}
	packet := map[string]any{"source_key": result.Plan.Ticket["source_key"], "historical_commit": result.Plan.Commit, "prediction": prediction, "candidate": repair.Candidate, "documents": documents, "before": repair.Before, "after": repair.After, "runtime_gate": "behavior_verified"}
	client, e := newClient(Target{URL: m.Evaluator.URL}, os.Getenv(m.Evaluator.TokenEnv))
	if e != nil {
		return nil, e
	}
	response, e := client.request(ctx, m.Evaluator.Request, map[string]any{"evaluation_packet": packet})
	if e != nil {
		return nil, e
	}
	if response.Status >= 300 {
		return nil, fmt.Errorf("external evaluator returned HTTP %d", response.Status)
	}
	body, ok := response.Body.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("evaluator must return an object")
	}
	assessments, ok := body["findings"].([]any)
	if !ok || len(assessments) != len(prediction.Findings) {
		return nil, fmt.Errorf("evaluator must assess every observed finding")
	}
	seen := map[int]bool{}
	for _, value := range assessments {
		assessment, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid semantic finding")
		}
		index, ok := assessment["index"].(float64)
		i := int(index)
		if !ok || float64(i) != index || i < 0 || i >= len(prediction.Findings) || seen[i] {
			return nil, fmt.Errorf("invalid finding index")
		}
		seen[i] = true
		for _, key := range []string{"localization", "problem", "solution"} {
			state := text(assessment[key])
			if state != "correct" && state != "incorrect" && state != "unknown" {
				return nil, fmt.Errorf("invalid semantic assessment")
			}
		}
		if text(assessment["explanation"]) == "" {
			return nil, fmt.Errorf("semantic assessment requires explanation")
		}
	}
	evidence, ok := body["evidence"].([]any)
	if !ok || len(evidence) == 0 {
		return nil, fmt.Errorf("evaluator requires supplied evidence citations")
	}
	for _, value := range evidence {
		ev, _ := value.(map[string]any)
		valid := false
		quote := text(ev["quote"])
		ref := text(ev["reference"])
		for _, d := range documents {
			if text(ev["source"]) != d.Source || !strings.HasPrefix(ref, d.Prefix) || strings.TrimSpace(quote) == "" {
				continue
			}
			line, e := strconv.Atoi(strings.TrimPrefix(ref, d.Prefix))
			i := line - d.First
			if e == nil && i >= 0 && i < len(d.Lines) && strings.Contains(d.Lines[i], quote) {
				valid = true
			}
		}
		if !valid {
			return nil, fmt.Errorf("evaluator quote is not supplied evidence: %s", ref)
		}
	}
	return response.Body, nil
}
