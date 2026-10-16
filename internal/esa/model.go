package esa

import (
	"encoding/json"
	"time"

	"esa/internal/classifier"
)

// Cache/task contract revision; old queued contracts require a fresh forwarding.
const WorkflowVersion = "10"

// Comment retains source timestamps so historical snapshots can reject future discussion.
type Comment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Resolution separates cause, proposed/applied correction and validation text.
// A populated value does not itself establish engineering approval.
type Resolution struct {
	Reason      string `json:"reason"`
	Cause       string `json:"cause"`
	Solution    string `json:"solution"`
	Validation  string `json:"validation"`
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

// Ticket is the diagnostic snapshot accepted by ESA, not a live Redmine object.
// ForwardedAt defines the available knowledge cutoff; labels never gate intake.
type Ticket struct {
	StoredFields JSONFields        `json:"-"`
	ForwardedBy  string            `json:"forwarded_by,omitempty"`
	ForwardedAt  time.Time         `json:"forwarded_at,omitempty"`
	ID           string            `json:"id"`
	SourceKey    string            `json:"source_key,omitempty"`
	ProjectID    string            `json:"project_id"`
	Project      string            `json:"project,omitempty"`
	Subject      string            `json:"subject"`
	Description  string            `json:"description"`
	Tracker      string            `json:"tracker"`
	Status       string            `json:"status"`
	Assignee     string            `json:"assignee,omitempty"`
	Source       string            `json:"source"`
	Components   []string          `json:"components"`
	Versions     []string          `json:"versions"`
	Environment  map[string]string `json:"environment"`
	ErrorCodes   []string          `json:"error_codes"`
	Comments     []Comment         `json:"comments"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Closed       bool              `json:"closed"`
	Private      bool              `json:"private"`
	Resolution   Resolution        `json:"resolution"`
}

// Event asserts an explicit human forwarding action and carries its frozen ticket.
// Its ID is idempotent only when content and force semantics are unchanged.
type Event struct {
	ForwardedBy string          `json:"forwarded_by"`
	ForwardedAt time.Time       `json:"forwarded_at"`
	Force       bool            `json:"force,omitempty"`
	ID          string          `json:"id"`
	Origin      string          `json:"origin"`
	OccurredAt  time.Time       `json:"occurred_at"`
	ReceivedAt  time.Time       `json:"received_at"`
	Ticket      Ticket          `json:"ticket"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

type Config struct {
	Version    string           `json:"version"`
	Budget     Budget           `json:"budget"`
	Classifier ClassifierConfig `json:"classifier"`
	Worker     WorkerConfig     `json:"worker"`
	Replay     ReplayConfig     `json:"-"`
}

type ClassifierConfig struct {
	Enabled bool              `json:"enabled"`
	Models  map[string]string `json:"models,omitempty"`
}

type ClassifierPrediction struct {
	ID          string                `json:"id"`
	TicketID    string                `json:"ticket_id"`
	ProjectID   string                `json:"project_id"`
	Text        string                `json:"text"`
	ForwardedAt time.Time             `json:"forwarded_at"`
	CreatedAt   time.Time             `json:"created_at"`
	Prediction  classifier.Prediction `json:"prediction"`
}

type ClassifierFeedback struct {
	ID           string             `json:"id"`
	PredictionID string             `json:"prediction_id"`
	ModelVersion string             `json:"model_version"`
	Note         string             `json:"note"`
	Example      classifier.Example `json:"example"`
}

// Budget separates enforced call/deadline limits from advisory token thresholds.
// MaxCalls applies per stage; cached input is a subset of input, not extra usage.
type Budget struct {
	MaxCalls        int           `json:"max_calls"`
	MaxInputTokens  int           `json:"max_input_tokens"`
	MaxOutputTokens int           `json:"max_output_tokens"`
	TimeoutSeconds  int           `json:"timeout_seconds"`
	WarnTotalTokens int           `json:"warn_total_tokens,omitempty"`
	TokenWeights    *TokenWeights `json:"token_weights,omitempty"`
}

// Weights express advisory credit-equivalent units, not a provider price quote.
// Cached input is a subset of input; reasoning is already included in output.
type TokenWeights struct {
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cached_input"`
	Output      float64 `json:"output"`
}

func (w *TokenWeights) UnmarshalJSON(data []byte) error {
	type weights TokenWeights
	value := weights{Input: 1, CachedInput: 0.1, Output: 1}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*w = TokenWeights(value)
	return nil
}

// Case is a revisioned resolution collected from a forwarded closed ticket.
// Draft collection and trusted engineer review are separate transitions.
type Case struct {
	SolutionID   string            `json:"solution_id,omitempty"`
	StoredFields JSONFields        `json:"-"`
	IssueID      string            `json:"engineering_issue_id,omitempty"`
	ID           string            `json:"id"`
	SourceHash   string            `json:"source_hash"`
	TicketID     string            `json:"ticket_id"`
	ProjectID    string            `json:"project_id"`
	SourceKey    string            `json:"source_key"`
	Subject      string            `json:"subject"`
	Summary      string            `json:"summary"`
	Components   []string          `json:"components"`
	Versions     []string          `json:"versions"`
	Environment  map[string]string `json:"environment"`
	Resolution   Resolution        `json:"resolution"`
	State        string            `json:"state"`
	ReviewedBy   string            `json:"reviewed_by,omitempty"`
	ReviewNote   string            `json:"review_note,omitempty"`
	Version      int               `json:"version"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	TrustedAt    time.Time         `json:"trusted_at,omitempty"`
}

// Usage retains raw provider counts. TokensReported=false means unknown, not zero.
type Usage struct {
	Calls             int  `json:"calls"`
	InputTokens       int  `json:"input_tokens"`
	CachedInputTokens int  `json:"cached_input_tokens,omitempty"`
	OutputTokens      int  `json:"output_tokens"`
	TokensReported    bool `json:"tokens_reported"`
}

// Evidence binds an exact quote to a source reference; it does not prove causality.
type Evidence struct {
	Source    string `json:"source"`
	Reference string `json:"reference"`
	Quote     string `json:"quote"`
}

// Finding pins localization to an immutable source revision.
type Finding struct {
	Commit    string     `json:"commit"`
	File      string     `json:"file"`
	StartLine int        `json:"start_line"`
	EndLine   int        `json:"end_line"`
	Problem   string     `json:"problem"`
	Solution  string     `json:"solution"`
	Evidence  []Evidence `json:"evidence"`
}
type IssueAssignment struct {
	ExistingID string `json:"existing_id"`
	Title      string `json:"title"`
	Reason     string `json:"reason"`
}

// Engineering requests grouping; only routing selects the authoritative Issue.
type IssueRequest struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// Report combines immutable diagnosis, routing provenance, usage and limitations.
// Engineering requests grouping; only Routing supplies the accepted Issue decision.
type Report struct {
	IssueRequest        *IssueRequest        `json:"issue_request,omitempty"`
	AssignmentRouting   *RoutingResult       `json:"assignment_routing,omitempty"`
	AccessedIssueIDs    []string             `json:"accessed_issue_ids,omitempty"`
	StoredFields        JSONFields           `json:"-"`
	Routing             *RoutingResult       `json:"routing,omitempty"`
	Envelope            *ReportEnvelope      `json:"assessment,omitempty"`
	Plan                *AnalysisPlan        `json:"plan,omitempty"`
	Issue               *IssueAssignment     `json:"issue,omitempty"`
	Findings            []Finding            `json:"findings,omitempty"`
	Summary             string               `json:"summary"`
	RootCauseCandidates []string             `json:"root_cause_candidates"`
	Evidence            []Evidence           `json:"evidence"`
	Recommendations     []string             `json:"recommendations"`
	VerificationSteps   []string             `json:"verification_steps"`
	Limitations         []string             `json:"limitations"`
	Usage               Usage                `json:"usage"`
	Alerts              []Alert              `json:"alerts,omitempty"`
	EnvironmentRequests []EnvironmentRequest `json:"environment_requests,omitempty"`
}

type Alert struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EnvironmentRequest proposes an operator-reviewed environment correction.
// Recording it grants no permission to patch the product or apply the change.
type EnvironmentRequest struct {
	Reason         string `json:"reason"`
	ProposedChange string `json:"proposed_change"`
	Validation     string `json:"validation"`
	State          string `json:"state"`
}

// Run records one analysis attempt against immutable input and shared investigation state.
type Run struct {
	StoredFields       JSONFields `json:"-"`
	SourceTicketID     string     `json:"source_ticket_id,omitempty"`
	InvestigationID    string     `json:"investigation_id,omitempty"`
	SnapshotID         string     `json:"snapshot_id,omitempty"`
	ObservationCutoff  time.Time  `json:"observation_cutoff,omitempty"`
	IssueID            string     `json:"issue_id,omitempty"`
	ID                 string     `json:"id"`
	TicketID           string     `json:"ticket_id"`
	ProjectID          string     `json:"project_id"`
	InputHash          string     `json:"input_hash"`
	WorkflowVersion    string     `json:"workflow_version"`
	RuleVersion        string     `json:"rule_version"`
	Input              Ticket     `json:"input"`
	Route              string     `json:"route"`
	State              string     `json:"state"`
	MissingInformation []string   `json:"missing_information"`
	Report             Report     `json:"report"`
	Error              string     `json:"error,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// EventRejection records an outbox event acknowledged without being accepted.
// It keeps no ticket body; the forwarder must correct the issue and forward again.
type EventRejection struct {
	EventID     string    `json:"event_id"`
	TicketID    string    `json:"ticket_id"`
	ProjectID   string    `json:"project_id"`
	ForwardedBy string    `json:"forwarded_by"`
	ForwardedAt time.Time `json:"forwarded_at"`
	Reason      string    `json:"reason"`
	RejectedAt  time.Time `json:"rejected_at"`
}

type Receipt struct {
	EventID  string `json:"event_id"`
	TicketID string `json:"ticket_id"`
	RunID    string `json:"run_id,omitempty"`
	CaseID   string `json:"case_id,omitempty"`
	Action   string `json:"action"`
}

// State is the persistence frame for business data and immutable audit history.
// Generated Issue-directory files are projections, never a second authority.
type State struct {
	IssueAccessSequence   uint64                          `json:"issue_access_sequence,omitempty"`
	StoredFields          JSONFields                      `json:"-"`
	LatestSnapshot        map[string]string               `json:"latest_snapshot,omitempty"`
	Snapshots             map[string]TicketSnapshot       `json:"snapshots,omitempty"`
	Issues                map[string]EngineeringIssue     `json:"engineering_issues,omitempty"`
	Memberships           map[string]IssueTicketLink      `json:"issue_ticket_links,omitempty"`
	MembershipHistory     []IssueTicketLink               `json:"issue_ticket_link_history,omitempty"`
	IssueOverrides        map[string]string               `json:"issue_overrides,omitempty"`
	Investigations        map[string]Investigation        `json:"investigations,omitempty"`
	Groups                map[string]InvestigationGroup   `json:"investigation_groups,omitempty"`
	Solutions             map[string]SolutionRevision     `json:"solution_revisions,omitempty"`
	ReplayClock           time.Time                       `json:"replay_clock,omitempty"`
	LastClaimedProject    string                          `json:"last_claimed_project,omitempty"`
	ClassifierPredictions map[string]ClassifierPrediction `json:"classifier_predictions,omitempty"`
	ClassifierFeedback    map[string]ClassifierFeedback   `json:"classifier_feedback,omitempty"`
	SchemaVersion         int                             `json:"schema_version"`
	Tickets               map[string]Ticket               `json:"tickets"`
	Events                map[string]Event                `json:"events"`
	Receipts              map[string]Receipt              `json:"receipts"`
	Rejections            map[string]EventRejection       `json:"event_rejections,omitempty"`
	Runs                  map[string]Run                  `json:"runs"`
	Latest                map[string]string               `json:"latest"`
	Cases                 map[string]Case                 `json:"cases"`
	CaseHistory           []Case                          `json:"case_history"`
}
