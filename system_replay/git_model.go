// Package systemreplay is a standalone external system replay tester.
package systemreplay

type Issue struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Category   string    `json:"category"`
	Title      string    `json:"title"`
	TicketKeys []string  `json:"ticket_keys"`
	Findings   []Finding `json:"findings"`
	CreatedAt  string    `json:"created_at"`
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

type Finding struct {
	Commit    string `json:"commit"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Problem   string `json:"problem"`
	Solution  string `json:"solution"`
}
