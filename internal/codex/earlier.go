package codex

import (
	"fmt"

	"esa/internal/esa"
)

// earlierInvestigation is what engineering sees of an unresolved record the
// Routing Agent matched to this ticket: where the earlier attempt looked and
// why it found no supported answer. It is a starting point, never evidence.
type earlierInvestigation struct {
	SourceKey   string   `json:"source_key"`
	Subject     string   `json:"subject"`
	Versions    []string `json:"versions"`
	Summary     string   `json:"summary"`
	Findings    []string `json:"findings,omitempty"`
	Limitations []string `json:"limitations"`
}

func earlierFrom(r esa.IssueRecord) *earlierInvestigation {
	e := &earlierInvestigation{SourceKey: r.SourceKey, Subject: r.Title, Versions: r.Versions, Summary: r.Summary, Limitations: r.Limitations}
	for _, f := range r.Findings {
		e.Findings = append(e.Findings, fmt.Sprintf("%s:%d-%d at %s: %s", f.File, f.StartLine, f.EndLine, short(f.Commit), f.Problem))
	}
	return e
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
