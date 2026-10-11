package backtest

import (
	"esa/internal/esa"
	"fmt"
	"sort"
	"strings"
)

// Assign groups only by explicit Agent assignment. Similar keywords are hints,
// not proof that two tickets share the same engineering issue.
func Assign(issues []Issue, ticket esa.Ticket, prediction esa.Report) ([]Issue, Issue, error) {
	if prediction.AgentClassification == nil || prediction.Issue == nil {
		return issues, Issue{}, fmt.Errorf("a structured engineering report is required")
	}
	category := prediction.AgentClassification.Category
	index := -1
	if prediction.Issue.ExistingID != "" {
		for i, issue := range issues {
			if issue.ID == prediction.Issue.ExistingID && issue.ProjectID == ticket.ProjectID && issue.Category == category {
				index = i
			}
		}
		if index < 0 {
			return issues, Issue{}, fmt.Errorf("assigned issue is absent or outside project/category")
		}
	} else {
		for _, existing := range issues {
			if existing.ID == "issue_"+ticket.SourceKey {
				return issues, Issue{}, fmt.Errorf("ticket already has an issue; assign its existing ID explicitly")
			}
		}
		issues = append(issues, Issue{ID: "issue_" + ticket.SourceKey, ProjectID: ticket.ProjectID, Category: category, Title: prediction.Issue.Title, CreatedAt: ticket.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")})
		index = len(issues) - 1
	}
	item := issues[index]
	found := false
	for _, key := range item.TicketKeys {
		if key == ticket.SourceKey {
			found = true
		}
	}
	if !found {
		item.TicketKeys = append(item.TicketKeys, ticket.SourceKey)
	}
	item.Findings = append(item.Findings, prediction.Findings...)
	issues[index] = item
	return issues, item, nil
}
func ReviewPacket(g Git, trace Trace, prediction esa.Report, sourceKey string, c Candidate) (ReviewRequest, error) {
	r := ReviewRequest{SourceKey: sourceKey, Issue: trace.Issue, Baseline: trace.Baseline, Prediction: prediction, Candidate: c, Documents: []Document{}}
	add := func(source, prefix, text string) {
		r.Documents = append(r.Documents, Document{source, prefix, 1, strings.Split(text, "\n")})
	}
	add("git", c.Commit+":message:", c.Message)
	add("patch", c.Commit+":patch:", c.Patch)
	// Numbered before/after windows enable exact citations without exposing a repo
	// or host tools to the retrospective reviewer.
	seen := map[string]bool{}
	for _, ch := range c.Changes {
		for _, x := range []struct {
			commit, file string
			start, count int
		}{{c.Parent, ch.OldFile, ch.OldStart, ch.OldCount}, {c.Commit, ch.NewFile, ch.NewStart, ch.NewCount}} {
			if x.file == "/dev/null" || x.file == "" {
				continue
			}
			key := x.commit + ":" + x.file + ":" + fmt.Sprint(x.start)
			if seen[key] {
				continue
			}
			seen[key] = true
			text, e := g.run("show", x.commit+":"+x.file)
			if e != nil {
				return r, e
			}
			lines := strings.Split(text, "\n")
			first, last := x.start-8, x.start+x.count+8
			if first < 1 {
				first = 1
			}
			if last > len(lines) {
				last = len(lines)
			}
			if last < first {
				continue
			}
			r.Documents = append(r.Documents, Document{"git", x.commit + ":" + x.file + ":", first, lines[first-1 : last]})
		}
	}
	return r, nil
}
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
func Evaluate(sourceKey string, trace Trace, prediction esa.Report, reviews []Review) (Evaluation, error) {
	e := Evaluation{SourceKey: sourceKey, IssueID: trace.Issue.ID, State: trace.State, Reviews: reviews, ResolvedCommits: []string{}, Findings: []FindingReview{}, Limitations: append([]string{}, trace.Limitations...)}
	for i := range prediction.Findings {
		e.Findings = append(e.Findings, FindingReview{i, "unknown", "unknown", "unknown", "No confirmed repair assessment yet."})
	}
	byCommit := map[string]Candidate{}
	for _, c := range trace.Candidates {
		byCommit[c.Commit] = c
	}
	positions := map[string]int{}
	for i, c := range trace.Candidates {
		positions[c.Commit] = i
	}
	reviews = append([]Review(nil), reviews...)
	sort.SliceStable(reviews, func(i, j int) bool { return positions[reviews[i].Commit] < positions[reviews[j].Commit] })
	e.Reviews = reviews
	conflicted := map[int]bool{}
	repairState := "unresolved"
	seen := map[string]bool{}
	for _, r := range reviews {
		c, ok := byCommit[r.Commit]
		if !ok || seen[r.Commit] {
			return e, fmt.Errorf("unknown or duplicate review commit")
		}
		seen[r.Commit] = true
		_ = c // Saved packets are validated by the CLI before aggregation.
		if r.Verdict == "regressed" {
			repairState = "reopened"
		}
		if r.Verdict == "resolved" {
			repairState = "resolved"
			e.ResolvedCommits = append(e.ResolvedCommits, r.Commit)
			for _, f := range r.Findings {
				if f.Index < 0 || f.Index >= len(e.Findings) {
					return e, fmt.Errorf("invalid reviewed finding")
				}
				if conflicted[f.Index] {
					continue
				}
				prior := e.Findings[f.Index]
				if prior.Localization == "unknown" && prior.Problem == "unknown" && prior.Solution == "unknown" {
					e.Findings[f.Index] = f
				} else if prior.Localization != f.Localization || prior.Problem != f.Problem || prior.Solution != f.Solution {
					conflicted[f.Index] = true
					e.Findings[f.Index] = FindingReview{f.Index, "unknown", "unknown", "unknown", "Confirmed repairs disagree about the initial finding; engineer review is required."}
				}
			}
		}
	}
	e.Pending = len(trace.Candidates) - len(seen)
	if len(e.ResolvedCommits) > 0 {
		e.State = repairState
	} else if len(trace.Candidates) > 0 {
		e.State = "unresolved"
	}
	for _, f := range e.Findings {
		if f.Localization == "incorrect" || f.Problem == "incorrect" || f.Solution == "incorrect" {
			e.Incorrect++
		} else if f.Localization == "correct" && f.Problem == "correct" && f.Solution == "correct" {
			e.Correct++
		} else {
			e.Unknown++
		}
	}
	if prediction.AgentClassification == nil {
		e.State = "no_prediction"
	} else if prediction.AgentClassification.Category == "unknown" {
		e.State = "classification_unknown"
	} else if engineering(prediction.AgentClassification.Category) && len(prediction.Findings) == 0 {
		e.State = "no_localization"
		if len(e.ResolvedCommits) > 0 && repairState == "resolved" {
			e.State = "resolved_without_localization"
		}
	} else if !engineering(prediction.AgentClassification.Category) {
		e.State = "customer_support"
	}
	e.Limitations = append(e.Limitations, "Correctness is an Agent assessment supported by the cited PR, patch and optional runtime evidence; it is not an archive-wide accuracy estimate.")
	return e, nil
}
func engineering(c string) bool {
	return c == "code_defect" || c == "compatibility" || c == "performance"
}
