package esa

import "time"

// Worker settings control task ownership and project concurrency.
type WorkerConfig struct {
	LeaseSeconds        int `json:"lease_seconds"`
	MaxActivePerProject int `json:"max_active_per_project"`
}

// Replay is supplied explicitly by the test launcher, never by business rules.
type ReplayConfig struct {
	Enabled     bool
	InitialTime time.Time
	Projects    []string
}

type TicketSnapshot struct {
	ID          string    `json:"id"`
	Hash        string    `json:"hash"`
	EventID     string    `json:"event_id"`
	Ticket      Ticket    `json:"ticket"`
	EffectiveAt time.Time `json:"effective_at"`
	AvailableAt time.Time `json:"available_at"`
	RecordedAt  time.Time `json:"recorded_at"`
	Cutoff      time.Time `json:"observation_cutoff"`
}
type EngineeringIssue struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Title     string    `json:"title"`
	State     string    `json:"state,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
type IssueTicketLink struct {
	Version         int       `json:"version"`
	Reviewer        string    `json:"reviewer,omitempty"`
	ID              string    `json:"id"`
	IssueID         string    `json:"issue_id"`
	TicketID        string    `json:"ticket_id"`
	SnapshotID      string    `json:"snapshot_id"`
	InvestigationID string    `json:"investigation_id,omitempty"`
	State           string    `json:"state"`
	Basis           string    `json:"basis"`
	AvailableAt     time.Time `json:"available_at"`
}
type MembershipReview struct {
	ExpectedSnapshotID string `json:"expected_snapshot_id"`
	IssueID            string `json:"issue_id"`
	Reviewer           string `json:"reviewer"`
	Reason             string `json:"reason"`
}
type InvestigationGroup struct {
	ID            string    `json:"id"`
	IssueID       string    `json:"issue_id"`
	ProjectID     string    `json:"project_id"`
	Signature     string    `json:"signature"`
	ScopeHash     string    `json:"scope_hash"`
	CurrentTaskID string    `json:"current_task_id"`
	CreatedAt     time.Time `json:"created_at"`
}
type TaskContract struct {
	ReviewContext      string    `json:"review_context,omitempty"`
	Objective          string    `json:"objective"`
	SnapshotID         string    `json:"snapshot_id"`
	InputHash          string    `json:"input_hash"`
	ObservationCutoff  time.Time `json:"observation_cutoff"`
	IssueID            string    `json:"issue_id"`
	CompletionCriteria []string  `json:"completion_criteria"`
	ReadOnly           bool      `json:"read_only"`
}
type Investigation struct {
	ID                string       `json:"id"`
	GroupID           string       `json:"group_id"`
	IssueID           string       `json:"issue_id"`
	ProjectID         string       `json:"project_id"`
	RunID             string       `json:"run_id"`
	State             string       `json:"state"`
	Contract          TaskContract `json:"contract"`
	Owner             string       `json:"owner,omitempty"`
	Epoch             int          `json:"epoch"`
	LeaseUntil        time.Time    `json:"lease_until,omitempty"`
	HeartbeatAt       time.Time    `json:"heartbeat_at,omitempty"`
	InvocationStarted bool         `json:"invocation_started"`
	SolutionID        string       `json:"solution_id,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
	ElapsedMS         int64        `json:"elapsed_ms"`
}
type SolutionRevision struct {
	ID              string     `json:"id"`
	IssueID         string     `json:"issue_id"`
	InvestigationID string     `json:"investigation_id"`
	ProjectID       string     `json:"project_id"`
	ScopeHash       string     `json:"scope_hash"`
	SourceTicketID  string     `json:"source_ticket_id"`
	Origin          string     `json:"origin"`
	State           string     `json:"state"`
	Revision        int        `json:"revision"`
	Supersedes      string     `json:"supersedes,omitempty"`
	RunID           string     `json:"run_id,omitempty"`
	Report          *Report    `json:"report,omitempty"` // Historical inline reports; new revisions reference RunID.
	Resolution      Resolution `json:"resolution"`
	Reviewer        string     `json:"reviewer,omitempty"`
	Note            string     `json:"note,omitempty"`
	AvailableAt     time.Time  `json:"available_at"`
	RecordedAt      time.Time  `json:"recorded_at"`
}
type SolutionReview struct {
	ExpectedSolutionID string     `json:"expected_solution_id"`
	Action             string     `json:"action"`
	Reviewer           string     `json:"reviewer"`
	Note               string     `json:"note"`
	Resolution         Resolution `json:"resolution"`
}
