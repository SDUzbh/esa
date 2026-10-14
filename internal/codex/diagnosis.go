package codex

import (
	"encoding/json"
	"errors"
	"esa/internal/esa"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EngineeringOutput is the model contract. Display fields are derived locally.
type EngineeringOutput struct {
	IssueRequest esa.IssueRequest `json:"issue_request"`
	Findings     []esa.Finding    `json:"findings"`
	Limitations  []string         `json:"limitations"`
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
func EngineeringSchema(sources ...string) []byte {
	integer := map[string]any{"type": "integer", "minimum": 1}
	finding := object(map[string]any{"commit": str(), "file": str(), "start_line": integer, "end_line": integer, "problem": str(), "solution": str(), "evidence": array(evidenceSchema(sources...))})
	schema := object(map[string]any{"issue_request": object(map[string]any{"title": str(), "reason": str()}), "findings": array(finding), "limitations": array(str())})
	b, _ := json.Marshal(schema)
	return b
}
func (out EngineeringOutput) Report(packet Packet) (esa.Report, error) {
	if strings.TrimSpace(out.IssueRequest.Title) == "" || strings.TrimSpace(out.IssueRequest.Reason) == "" {
		return esa.Report{}, errors.New("issue request title and reason are required")
	}
	r := esa.Report{IssueRequest: &out.IssueRequest, Findings: out.Findings, Summary: out.IssueRequest.Title, Limitations: out.Limitations}
	for _, f := range out.Findings {
		if f.Commit != packet.Commit || f.File == "" || strings.Contains(f.File, ":") || f.StartLine < 1 || f.EndLine < f.StartLine || f.EndLine-f.StartLine >= 200 || strings.TrimSpace(f.Problem) == "" || strings.TrimSpace(f.Solution) == "" || len(f.Evidence) == 0 {
			return r, errors.New("finding requires historical commit, file, bounded lines, problem, solution and evidence")
		}
		for _, line := range []int{f.StartLine, f.EndLine} {
			found := false
			for _, d := range packet.Documents {
				if d.Source == "git" && d.Prefix == f.Commit+":"+f.File+":" && line >= d.First && line < d.First+len(d.Lines) {
					found = true
				}
			}
			if !found {
				return r, fmt.Errorf("finding line was not inspected in historical source: %s:%d", f.File, line)
			}
		}
		sourceQuote := false
		for _, ev := range f.Evidence {
			if ev.Source == "git" && strings.HasPrefix(ev.Reference, f.Commit+":"+f.File+":") {
				line, e := strconv.Atoi(strings.TrimPrefix(ev.Reference, f.Commit+":"+f.File+":"))
				if e == nil && line >= f.StartLine && line <= f.EndLine {
					sourceQuote = true
				}
			}
		}
		if !sourceQuote {
			return r, errors.New("each finding must cite its historical source file")
		}
		r.RootCauseCandidates = append(r.RootCauseCandidates, f.Problem)
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
