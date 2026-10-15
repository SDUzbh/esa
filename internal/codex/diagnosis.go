package codex

import (
	"encoding/json"
	"errors"
	"esa/internal/esa"
	"esa/internal/replaytools"
	"sort"
	"strconv"
	"strings"
)

// EngineeringOutput is the model contract. Display fields are derived locally.
type EngineeringOutput struct {
	IssueRequest esa.IssueRequest     `json:"issue_request"`
	Findings     []EngineeringFinding `json:"findings"`
	Limitations  []string             `json:"limitations"`
}

// The Agent selects inspected evidence; source coordinates are derived locally.
type EngineeringFinding struct {
	Location    string   `json:"location"`
	EndLocation string   `json:"end_location"`
	Problem     string   `json:"problem"`
	Solution    string   `json:"solution"`
	EvidenceIDs []string `json:"evidence_ids"`
}

func visibleDocuments(docs []Document) []replaytools.NumberedDocument {
	result := make([]replaytools.NumberedDocument, 0, len(docs))
	for _, doc := range docs {
		result = append(result, replaytools.Numbered(doc))
	}
	return result
}

func object(properties map[string]any) map[string]any {
	required := make([]string, 0, len(properties))
	for key := range properties {
		required = append(required, key)
	}
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func str() map[string]any                  { return map[string]any{"type": "string"} }
func enum(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
func array(items any) map[string]any       { return map[string]any{"type": "array", "items": items} }
func evidenceSchema(sources ...string) map[string]any {
	source := str()
	if len(sources) > 0 {
		source = enum(sources...)
	}
	return object(map[string]any{"source": source, "reference": str(), "quote": str()})
}
func EngineeringSchema() []byte {
	finding := object(map[string]any{"location": str(), "end_location": str(), "problem": str(), "solution": str(), "evidence_ids": array(str())})
	schema := object(map[string]any{"issue_request": object(map[string]any{"title": str(), "reason": str()}), "findings": array(finding), "limitations": array(str())})
	b, _ := json.Marshal(schema)
	return b
}
func (out EngineeringOutput) Report(packet Packet) (esa.Report, error) {
	if strings.TrimSpace(out.IssueRequest.Title) == "" || strings.TrimSpace(out.IssueRequest.Reason) == "" {
		return esa.Report{}, errors.New("issue request title and reason are required")
	}
	r := esa.Report{IssueRequest: &out.IssueRequest, Summary: out.IssueRequest.Title, Limitations: out.Limitations}
	evidence := map[string]esa.Evidence{}
	for _, d := range packet.Documents {
		for i, text := range d.Lines {
			line := d.First + i
			id := replaytools.EvidenceID(d.Source, d.Prefix, line, text)
			ev := esa.Evidence{Source: d.Source, Reference: d.Prefix + strconv.Itoa(line), Quote: text}
			if previous, exists := evidence[id]; exists && previous != ev {
				return r, errors.New("ambiguous evidence ID")
			}
			evidence[id] = ev
		}
	}
	for _, selected := range out.Findings {
		begin, ok := evidence[selected.Location]
		end, endOK := evidence[selected.EndLocation]
		if !ok || !endOK || begin.Source != "git" || end.Source != "git" {
			return r, errors.New("finding locations must select inspected Git evidence IDs")
		}
		parts := strings.Split(begin.Reference, ":")
		if len(parts) != 3 || !strings.HasPrefix(end.Reference, parts[0]+":"+parts[1]+":") {
			return r, errors.New("finding endpoints must select the same historical file")
		}
		first, firstErr := strconv.Atoi(parts[2])
		last, lastErr := strconv.Atoi(strings.TrimPrefix(end.Reference, parts[0]+":"+parts[1]+":"))
		if firstErr != nil || lastErr != nil {
			return r, errors.New("invalid source coordinates in evidence")
		}
		f := esa.Finding{Commit: parts[0], File: parts[1], StartLine: first, EndLine: last, Problem: selected.Problem, Solution: selected.Solution}
		seen := map[string]bool{}
		for _, id := range append([]string{selected.Location, selected.EndLocation}, selected.EvidenceIDs...) {
			if seen[id] {
				continue
			}
			seen[id] = true
			ev, exists := evidence[id]
			if !exists || strings.TrimSpace(ev.Quote) == "" {
				return r, errors.New("evidence ID is unknown or selects an empty line")
			}
			f.Evidence = append(f.Evidence, ev)
		}
		if f.Commit != packet.Commit || f.File == "" || strings.Contains(f.File, ":") || f.StartLine < 1 || f.EndLine < f.StartLine || f.EndLine-f.StartLine >= 200 || strings.TrimSpace(f.Problem) == "" || strings.TrimSpace(f.Solution) == "" || len(f.Evidence) == 0 {
			return r, errors.New("finding requires historical commit, file, bounded lines, problem, solution and evidence")
		}
		r.RootCauseCandidates = append(r.RootCauseCandidates, f.Problem)
		r.Findings = append(r.Findings, f)
		r.Recommendations = append(r.Recommendations, f.Solution)
		r.Evidence = append(r.Evidence, f.Evidence...)
	}
	if len(out.Findings) == 0 {
		r.Limitations = append(r.Limitations, "No concrete code location was established; localization remains unassessed.")
		return r, nil
	}
	r.VerificationSteps = []string{"Track subsequent mainline versions, review matching PRs and replay the reported scenario to assess the proposed solution."}
	return r, ValidateEvidence(r, packet)
}
