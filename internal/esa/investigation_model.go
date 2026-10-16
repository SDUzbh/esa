package esa

import "time"

// Worker settings control task ownership and project concurrency.
type WorkerConfig struct {
	LeaseSeconds        int `json:"lease_seconds"`
	MaxActivePerProject int `json:"max_active_per_project"`
	// MaxQueueWaitSeconds bounds waiting for an engineering slot, separately
	// from the per-stage Budget.TimeoutSeconds.
	MaxQueueWaitSeconds int `json:"max_queue_wait_seconds"`
}

// Replay is supplied explicitly by the test launcher, never by business rules.
type ReplayConfig struct {
	Enabled     bool
	InitialTime time.Time
	Projects    []string
}

// TicketSnapshot pins diagnostic input and its effective, available and recorded times.
// Cutoff stays fixed even when processing or review happens later.
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

// EngineeringIssue groups diagnosed problems independently of Redmine ticket identity.
// AccessOrder supplies durable LRU ordering when replay timestamps are equal.
type EngineeringIssue struct {
	LastAccessedAt      time.Time `json:"last_accessed_at,omitempty"`
	AccessOrder         uint64    `json:"access_order,omitempty"`
	TitleSourceTicketID string    `json:"title_source_ticket_id,omitempty"`
	TitleRestricted     bool      `json:"title_restricted,omitempty"`
	ID                  string    `json:"id"`
	ProjectID           string    `json:"project_id"`
	Title               string    `json:"title"`
	State               string    `json:"state,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
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

// MembershipReview uses expected snapshot/link IDs to reject stale human edits.
// Changing membership does not forward a ticket or start another analysis.
type MembershipReview struct {
	ExpectedMembershipID string `json:"expected_membership_id"`
	ExpectedSnapshotID   string `json:"expected_snapshot_id"`
	IssueID              string `json:"issue_id"`
	Reviewer             string `json:"reviewer"`
	Reason               string `json:"reason"`
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

// TaskContract fixes objective, input hash, cutoff and read-only completion criteria.
// Later solution knowledge cannot rewrite this initial analysis contract.
type TaskContract struct {
	StoredFields       JSONFields `json:"-"`
	Objective          string     `json:"objective"`
	SnapshotID         string     `json:"snapshot_id"`
	InputHash          string     `json:"input_hash"`
	ObservationCutoff  time.Time  `json:"observation_cutoff"`
	IssueID            string     `json:"issue_id"`
	CompletionCriteria []string   `json:"completion_criteria"`
	ReadOnly           bool       `json:"read_only"`
}

// Investigation owns execution status, lease and fencing epoch for shared work.
// Phase distinguishes routing, capacity waiting, engineering and assignment.
type Investigation struct {
	JoinedInvestigationID string               `json:"joined_investigation_id,omitempty"`
	Phase                 string               `json:"phase,omitempty"`
	Application           *SolutionApplication `json:"solution_application,omitempty"`
	ID                    string               `json:"id"`
	GroupID               string               `json:"group_id"`
	IssueID               string               `json:"issue_id"`
	ProjectID             string               `json:"project_id"`
	RunID                 string               `json:"run_id"`
	State                 string               `json:"state"`
	Contract              TaskContract         `json:"contract"`
	Owner                 string               `json:"owner,omitempty"`
	Epoch                 int                  `json:"epoch"`
	LeaseUntil            time.Time            `json:"lease_until,omitempty"`
	HeartbeatAt           time.Time            `json:"heartbeat_at,omitempty"`
	InvocationStarted     bool                 `json:"invocation_started"`
	SolutionID            string               `json:"solution_id,omitempty"`
	CreatedAt             time.Time            `json:"created_at"`
	UpdatedAt             time.Time            `json:"updated_at"`
	ElapsedMS             int64                `json:"elapsed_ms"`
}

// SolutionRevision is an immutable proposal or reviewed solution within one scope.
// Superseding publishes another revision instead of editing the original report.
type SolutionRevision struct {
	StoredFields    JSONFields `json:"-"`
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
	Resolution      Resolution `json:"resolution"`
	Reviewer        string     `json:"reviewer,omitempty"`
	Note            string     `json:"note,omitempty"`
	AvailableAt     time.Time  `json:"available_at"`
	RecordedAt      time.Time  `json:"recorded_at"`
}
type SolutionReview struct {
	ExpectedApplicationRevision int        `json:"expected_application_revision,omitempty"`
	ExpectedSolutionID          string     `json:"expected_solution_id"`
	Action                      string     `json:"action"`
	Reviewer                    string     `json:"reviewer"`
	Note                        string     `json:"note"`
	Resolution                  Resolution `json:"resolution"`
}

// An application references existing knowledge. Its applicability review does
// not publish another knowledge revision or change the source's trust status.
type SolutionApplication struct {
	SolutionID     string              `json:"solution_id,omitempty"`
	CaseID         string              `json:"case_id,omitempty"`
	CaseVersion    int                 `json:"case_version,omitempty"`
	SourceTicketID string              `json:"source_ticket_id"`
	State          string              `json:"state"`
	Revision       int                 `json:"revision"`
	Reviews        []ApplicationReview `json:"reviews,omitempty"`
}
type ApplicationReview struct {
	State       string    `json:"state"`
	Reviewer    string    `json:"reviewer"`
	Note        string    `json:"note"`
	AvailableAt time.Time `json:"available_at"`
}
