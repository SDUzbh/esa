package codexreview

import (
	"fmt"
	"strings"
)

func ValidateReview(r Review, request ReviewRequest) error {
	if r.Commit != request.Candidate.Commit || strings.TrimSpace(r.Explanation) == "" {
		return fmt.Errorf("review must identify the registered candidate and explain its verdict")
	}
	if r.Verdict != "resolved" && r.Verdict != "not_resolved" && r.Verdict != "regressed" && r.Verdict != "uncertain" {
		return fmt.Errorf("invalid repair verdict")
	}
	if len(request.Candidate.PRs) == 0 && r.Verdict == "resolved" {
		return fmt.Errorf("resolved verdict requires a PR reference")
	}
	if len(r.Findings) != len(request.Prediction.Findings) {
		return fmt.Errorf("review must assess every original finding")
	}
	seen := map[int]bool{}
	for _, f := range r.Findings {
		if f.Index < 0 || f.Index >= len(request.Prediction.Findings) || seen[f.Index] || f.Explanation == "" {
			return fmt.Errorf("invalid finding assessment")
		}
		seen[f.Index] = true
		for _, v := range []string{f.Localization, f.Problem, f.Solution} {
			if v != "correct" && v != "incorrect" && v != "unknown" {
				return fmt.Errorf("invalid accuracy judgment")
			}
		}
		if r.Verdict != "resolved" && (f.Localization != "unknown" || f.Problem != "unknown" || f.Solution != "unknown") {
			return fmt.Errorf("unconfirmed repair must leave initial accuracy unknown")
		}
	}
	if len(r.Evidence) == 0 {
		return fmt.Errorf("review must cite supplied evidence")
	}
	for _, ev := range r.Evidence {
		valid := false
		for _, d := range request.Documents {
			if ev.Source != d.Source || !strings.HasPrefix(ev.Reference, d.Prefix) || strings.TrimSpace(ev.Quote) == "" {
				continue
			}
			var line int
			tail := strings.TrimPrefix(ev.Reference, d.Prefix)
			if _, e := fmt.Sscanf(tail, "%d", &line); e == nil && tail == fmt.Sprint(line) {
				i := line - d.First
				if i >= 0 && i < len(d.Lines) && strings.Contains(d.Lines[i], ev.Quote) {
					valid = true
				}
			}
		}
		if !valid {
			return fmt.Errorf("review quote does not match supplied line: %s", ev.Reference)
		}
	}
	return nil
}
