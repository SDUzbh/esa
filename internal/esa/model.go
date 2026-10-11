package esa

import (
	"encoding/json"
	"time"
)

const WorkflowVersion = "1"

type Comment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Resolution struct {
	Reason      string `json:"reason"`
	Cause       string `json:"cause"`
	Solution    string `json:"solution"`
	Validation  string `json:"validation"`
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

type Ticket struct {
	ID               string            `json:"id"`
	SourceKey        string            `json:"source_key,omitempty"`
	ProjectID        string            `json:"project_id"`
	Project          string            `json:"project,omitempty"`
	Subject          string            `json:"subject"`
	Description      string            `json:"description"`
	Tracker          string            `json:"tracker"`
	Status           string            `json:"status"`
	Assignee         string            `json:"assignee,omitempty"`
	Source           string            `json:"source"`
	Components       []string          `json:"components"`
	Versions         []string          `json:"versions"`
	Environment      map[string]string `json:"environment"`
	ErrorCodes       []string          `json:"error_codes"`
	Comments         []Comment         `json:"comments"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Closed           bool              `json:"closed"`
	Private          bool              `json:"private"`
	Resolution       Resolution        `json:"resolution"`
	CategoryOverride string            `json:"category_override,omitempty"`
}

type Event struct {
	ID         string    `json:"id"`
	Origin     string    `json:"origin"`
	OccurredAt time.Time `json:"occurred_at"`
	ReceivedAt time.Time `json:"received_at"`
	Ticket     Ticket    `json:"ticket"`
	Raw        jsonRaw   `json:"raw,omitempty"`
}

type jsonRaw = json.RawMessage

type Rule struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Any      []string `json:"any"`
	All      []string `json:"all"`
	None     []string `json:"none"`
	Trackers []string `json:"trackers"`
}

type Config struct {
	Version               string   `json:"version"`
	EngineeringCategories []string `json:"engineering_categories"`
	Rules                 []Rule   `json:"rules"`
	Budget                Budget   `json:"budget"`
}

type Budget struct {
	MaxCalls        int `json:"max_calls"`
	MaxInputTokens  int `json:"max_input_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
	TimeoutSeconds  int `json:"timeout_seconds"`
	WarnTotalTokens int `json:"warn_total_tokens,omitempty"`
}

type Classification struct {
	Category   string   `json:"category"`
	RuleIDs    []string `json:"rule_ids"`
	Candidates []string `json:"candidates,omitempty"`
}

type Case struct {
	ID          string            `json:"id"`
	SourceHash  string            `json:"source_hash"`
	TicketID    string            `json:"ticket_id"`
	ProjectID   string            `json:"project_id"`
	SourceKey   string            `json:"source_key"`
	Subject     string            `json:"subject"`
	Summary     string            `json:"summary"`
	Category    string            `json:"category"`
	Components  []string          `json:"components"`
	Versions    []string          `json:"versions"`
	Environment map[string]string `json:"environment"`
	Signals     []string          `json:"signals"`
	Keywords    []string          `json:"keywords"`
	Resolution  Resolution        `json:"resolution"`
	State       string            `json:"state"`
	ReviewedBy  string            `json:"reviewed_by,omitempty"`
	ReviewNote  string            `json:"review_note,omitempty"`
	Version     int               `json:"version"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	TrustedAt   time.Time         `json:"trusted_at,omitempty"`
}

type Match struct {
	CaseID      string   `json:"case_id"`
	CaseVersion int      `json:"case_version"`
	Subject     string   `json:"subject"`
	Score       int      `json:"score"`
	Applicable  bool     `json:"applicable"`
	Reasons     []string `json:"reasons"`
	Solution    string   `json:"solution,omitempty"`
}

type Usage struct {
	Calls          int  `json:"calls"`
	InputTokens    int  `json:"input_tokens"`
	OutputTokens   int  `json:"output_tokens"`
	TokensReported bool `json:"tokens_reported"`
}

type Evidence struct {
	Source    string `json:"source"`
	Reference string `json:"reference"`
	Quote     string `json:"quote"`
}

type Report struct {
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

type EnvironmentRequest struct {
	Reason         string `json:"reason"`
	ProposedChange string `json:"proposed_change"`
	Validation     string `json:"validation"`
	State          string `json:"state"`
}

type Run struct {
	ID                 string         `json:"id"`
	TicketID           string         `json:"ticket_id"`
	ProjectID          string         `json:"project_id"`
	InputHash          string         `json:"input_hash"`
	WorkflowVersion    string         `json:"workflow_version"`
	RuleVersion        string         `json:"rule_version"`
	CaseGeneration     int            `json:"case_generation"`
	Input              Ticket         `json:"input"`
	Classification     Classification `json:"classification"`
	Route              string         `json:"route"`
	State              string         `json:"state"`
	Matches            []Match        `json:"matches"`
	MissingInformation []string       `json:"missing_information"`
	Report             Report         `json:"report"`
	Error              string         `json:"error,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

type Receipt struct {
	EventID  string `json:"event_id"`
	TicketID string `json:"ticket_id"`
	RunID    string `json:"run_id,omitempty"`
	CaseID   string `json:"case_id,omitempty"`
	Action   string `json:"action"`
}

type State struct {
	SchemaVersion int                `json:"schema_version"`
	Generation    int                `json:"generation"`
	Tickets       map[string]Ticket  `json:"tickets"`
	Events        map[string]Event   `json:"events"`
	Receipts      map[string]Receipt `json:"receipts"`
	Runs          map[string]Run     `json:"runs"`
	Latest        map[string]string  `json:"latest"`
	Cases         map[string]Case    `json:"cases"`
	CaseHistory   []Case             `json:"case_history"`
}
