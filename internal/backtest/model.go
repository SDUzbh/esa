// Package backtest follows ticket-centered issues through mainline versions.
package backtest

import "esa/internal/esa"

type Issue struct {
	ID         string        `json:"id"`
	ProjectID  string        `json:"project_id"`
	Category   string        `json:"category"`
	Title      string        `json:"title"`
	TicketKeys []string      `json:"ticket_keys"`
	Findings   []esa.Finding `json:"findings"`
	CreatedAt  string        `json:"created_at"`
}
type Version struct {
	Name           string `json:"name"`
	Commit         string `json:"commit"`
	MainlineCommit string `json:"mainline_commit"`
	CommittedAt    string `json:"committed_at"`
}
type Change struct {
	OldFile  string `json:"old_file"`
	NewFile  string `json:"new_file"`
	OldStart int    `json:"old_start"`
	OldCount int    `json:"old_count"`
	NewStart int    `json:"new_start"`
	NewCount int    `json:"new_count"`
}
type Candidate struct {
	Commit  string   `json:"commit"`
	Parent  string   `json:"parent"`
	Version string   `json:"version"`
	Subject string   `json:"subject"`
	Message string   `json:"message"`
	Reasons []string `json:"reasons"`
	PRs     []string `json:"prs"`
	Changes []Change `json:"changes"`
	Patch   string   `json:"patch"`
}
type Trace struct {
	Issue        Issue       `json:"issue"`
	Baseline     string      `json:"baseline"`
	Mainline     string      `json:"mainline"`
	MainlineHead string      `json:"mainline_head"`
	Versions     []Version   `json:"versions"`
	Candidates   []Candidate `json:"candidates"`
	State        string      `json:"state"`
	Limitations  []string    `json:"limitations"`
}
type ReviewRequest struct {
	SourceKey  string         `json:"source_key"`
	Issue      Issue          `json:"issue"`
	Baseline   string         `json:"baseline"`
	Prediction esa.Report     `json:"prediction"`
	Candidate  Candidate      `json:"candidate"`
	Documents  []Document     `json:"documents"`
	Runtime    map[string]any `json:"runtime,omitempty"`
}
type Document struct {
	Source string   `json:"source"`
	Prefix string   `json:"reference_prefix"`
	First  int      `json:"first_line"`
	Lines  []string `json:"lines"`
}
type FindingReview struct {
	Index        int    `json:"index"`
	Localization string `json:"localization"`
	Problem      string `json:"problem"`
	Solution     string `json:"solution"`
	Explanation  string `json:"explanation"`
}
type Review struct {
	Commit      string          `json:"commit"`
	Verdict     string          `json:"verdict"`
	Explanation string          `json:"explanation"`
	Findings    []FindingReview `json:"findings"`
	Evidence    []esa.Evidence  `json:"evidence"`
	Limitations []string        `json:"limitations"`
	Usage       esa.Usage       `json:"usage"`
	Alerts      []esa.Alert     `json:"alerts,omitempty"`
}
type Evaluation struct {
	SourceKey       string          `json:"source_key"`
	IssueID         string          `json:"issue_id"`
	State           string          `json:"state"`
	ResolvedCommits []string        `json:"resolved_commits"`
	Reviews         []Review        `json:"reviews"`
	Findings        []FindingReview `json:"findings"`
	Pending         int             `json:"pending_candidates"`
	Correct         int             `json:"correct_findings"`
	Incorrect       int             `json:"incorrect_findings"`
	Unknown         int             `json:"unknown_findings"`
	Limitations     []string        `json:"limitations"`
}
