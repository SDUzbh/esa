package systemreplay

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Command struct {
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env,omitempty"`
	Dir            string            `json:"dir,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}
type Extract struct {
	RunID    []string `json:"run_id"`
	State    []string `json:"state"`
	Category []string `json:"category"`
	Route    []string `json:"route"`
	Findings []string `json:"findings"`
	IssueID  []string `json:"issue_id"`
	Usage    []string `json:"usage"`
}
type Target struct {
	URL              string            `json:"url"`
	Start            Command           `json:"start"`
	HealthPath       string            `json:"health_path"`
	TokenEnv         string            `json:"token_env,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Submit           Request           `json:"submit"`
	Observe          Request           `json:"observe"`
	AdvanceClock     *Request          `json:"advance_clock,omitempty"`
	Prepare          *Request          `json:"prepare,omitempty"`
	PrepareCommand   *Command          `json:"prepare_command,omitempty"`
	Extract          Extract           `json:"extract"`
	Capture          map[string]string `json:"capture,omitempty"`
	TerminalStates   []string          `json:"terminal_states"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	PollMilliseconds int               `json:"poll_milliseconds"`
}
type Assertion struct {
	Path  string `json:"path"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}
type Event struct {
	SkipObserve   bool           `json:"skip_observe,omitempty"`
	At            string         `json:"at"`
	ID            string         `json:"id,omitempty"`
	Ticket        map[string]any `json:"ticket,omitempty"`
	Request       *Request       `json:"request,omitempty"`
	Observe       *Request       `json:"observe,omitempty"`
	Expect        []Assertion    `json:"expect"`
	ReceiptExpect []Assertion    `json:"receipt_expect,omitempty"`
}
type Scenario struct {
	Profile  string      `json:"profile"`
	Command  string      `json:"command,omitempty"`
	Controls []Assertion `json:"controls"`
	Bug      []Assertion `json:"bug"`
	Healthy  []Assertion `json:"healthy"`
}
type Job struct {
	ID          string            `json:"id"`
	Raw         string            `json:"raw,omitempty"`
	Ticket      map[string]any    `json:"ticket,omitempty"`
	At          string            `json:"at,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Version     string            `json:"version,omitempty"`
	Project     string            `json:"project,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Events      []Event           `json:"events,omitempty"`
	Expect      []Assertion       `json:"expect,omitempty"`
	Scenario    *Scenario         `json:"scenario,omitempty"`
}
type Evaluator struct {
	URL      string  `json:"url"`
	TokenEnv string  `json:"token_env,omitempty"`
	Request  Request `json:"request"`
}

// Manifest configures public requests, external assertions and optional runtime/evaluator inputs.
// Commands are trusted operator configuration, never generated from ticket text.
type Manifest struct {
	Evaluator        *Evaluator `json:"evaluator,omitempty"`
	Schema           int        `json:"schema_version"`
	Repo             string     `json:"repo,omitempty"`
	Mainline         string     `json:"mainline,omitempty"`
	Versions         int        `json:"versions,omitempty"`
	Until            string     `json:"until,omitempty"`
	Target           Target     `json:"target"`
	Runtime          Command    `json:"runtime"`
	Jobs             []Job      `json:"jobs"`
	WarnInputTokens  int        `json:"warn_input_tokens,omitempty"`
	WarnOutputTokens int        `json:"warn_output_tokens,omitempty"`
	WarnTotalTokens  int        `json:"warn_total_tokens,omitempty"`
	BaseDir          string     `json:"-"`
}
type Plan struct {
	Job         Job            `json:"job"`
	Ticket      map[string]any `json:"ticket"`
	Commit      string         `json:"commit,omitempty"`
	Version     string         `json:"version"`
	At          string         `json:"at"`
	RawHash     string         `json:"raw_sha256,omitempty"`
	Limitations []string       `json:"limitations"`
}
type HTTPRecord struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Body   any    `json:"body"`
}

// Observation stores properties extracted from a public response, not tester-generated decisions.
type Observation struct {
	RunID    string         `json:"run_id,omitempty"`
	State    string         `json:"state"`
	Category string         `json:"category"`
	Route    string         `json:"route"`
	IssueID  string         `json:"issue_id,omitempty"`
	Findings []Finding      `json:"findings,omitempty"`
	Usage    map[string]any `json:"usage,omitempty"`
	Raw      any            `json:"raw"`
}
type EventResult struct {
	At          string      `json:"at"`
	Receipt     HTTPRecord  `json:"receipt"`
	Observation Observation `json:"observation"`
	Checks      []Check     `json:"checks"`
	Error       string      `json:"error,omitempty"`
}
type Check struct {
	Assertion Assertion `json:"assertion"`
	Passed    bool      `json:"passed"`
	Actual    any       `json:"actual,omitempty"`
}
type RuntimeRecord struct {
	Commit       string         `json:"commit"`
	Setup        map[string]any `json:"setup"`
	Outcome      map[string]any `json:"outcome"`
	Controls     []Check        `json:"controls"`
	Bug          []Check        `json:"bug"`
	Healthy      []Check        `json:"healthy"`
	ScenarioHash string         `json:"scenario_sha256"`
}

// Repair records before/after behavior gates and optional independent semantic assessment.
// Patch overlap alone does not establish a correct initial diagnosis.
type Repair struct {
	SemanticAssessment any              `json:"semantic_assessment,omitempty"`
	Candidate          Candidate        `json:"candidate"`
	Before             RuntimeRecord    `json:"before"`
	After              RuntimeRecord    `json:"after"`
	State              string           `json:"state"`
	Localization       []map[string]any `json:"localization"`
	Error              string           `json:"error,omitempty"`
}
type Result struct {
	MeasuredUsage  map[string]int  `json:"measured_usage,omitempty"`
	UsageKeys      map[string]bool `json:"-"`
	ID             string          `json:"id"`
	State          string          `json:"state"`
	Plan           Plan            `json:"plan"`
	Events         []EventResult   `json:"events"`
	PredictionHash string          `json:"prediction_sha256,omitempty"`
	Baseline       *RuntimeRecord  `json:"baseline,omitempty"`
	Trace          *Trace          `json:"trace,omitempty"`
	Repairs        []Repair        `json:"repairs,omitempty"`
	Alerts         []string        `json:"alerts,omitempty"`
	Error          string          `json:"error,omitempty"`
}
type Summary struct {
	Error       string   `json:"error,omitempty"`
	Schema      int      `json:"schema_version"`
	Phase       string   `json:"phase"`
	Target      string   `json:"target"`
	Results     []Result `json:"results"`
	Cleanup     string   `json:"cleanup"`
	Limitations []string `json:"limitations"`
}

func load(path string) (Manifest, error) {
	var m Manifest
	b, e := os.ReadFile(path)
	if e != nil {
		return m, e
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(&m); e != nil {
		return m, e
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return m, fmt.Errorf("manifest must contain one JSON document")
	}
	m.BaseDir, _ = filepath.Abs(filepath.Dir(path))
	if m.Schema != 1 || len(m.Jobs) == 0 {
		return m, fmt.Errorf("schema_version 1 and jobs are required")
	}
	if m.Repo != "" {
		m.Repo = absolute(m.BaseDir, m.Repo)
	}
	if m.Mainline == "" {
		m.Mainline = "origin/trunk"
	}
	if m.Versions == 0 {
		m.Versions = 1
	}
	if m.Versions < 1 {
		return m, fmt.Errorf("versions must be positive")
	}
	if m.Target.HealthPath == "" {
		m.Target.HealthPath = "/healthz"
	}
	if m.Target.TimeoutSeconds == 0 {
		m.Target.TimeoutSeconds = 1000
	}
	if m.Target.PollMilliseconds == 0 {
		m.Target.PollMilliseconds = 250
	}
	if m.Target.TimeoutSeconds < 1 || m.Target.PollMilliseconds < 1 {
		return m, fmt.Errorf("positive target timeout and polling interval required")
	}
	ids := map[string]bool{}
	paths := map[string]bool{}
	for i, j := range m.Jobs {
		if j.ID == "" || ids[j.ID] || paths[safe(j.ID)] {
			return m, fmt.Errorf("unique job IDs and report paths are required")
		}
		ids[j.ID] = true
		paths[safe(j.ID)] = true
		if (j.Raw != "") == (j.Ticket != nil) {
			return m, fmt.Errorf("job %s requires exactly one raw input or ticket", j.ID)
		}
		if e := validateAssertions(j.Expect); e != nil {
			return m, e
		}
		for _, event := range j.Events {
			if event.SkipObserve && (len(event.Expect) > 0 || event.Observe != nil) {
				return m, fmt.Errorf("receipt-only event cannot specify observation assertions or request")
			}
			if event.Ticket != nil && text(event.Ticket["id"]) == "" {
				return m, fmt.Errorf("event ticket id required")
			}
			if e := validateAssertions(append(append([]Assertion{}, event.Expect...), event.ReceiptExpect...)); e != nil {
				return m, e
			}
		}
		if j.Scenario != nil {
			if j.Scenario.Profile == "" {
				return m, fmt.Errorf("scenario profile required")
			}
			for _, assertions := range [][]Assertion{j.Scenario.Controls, j.Scenario.Bug, j.Scenario.Healthy} {
				if e := validateAssertions(assertions); e != nil {
					return m, e
				}
			}
		}
		if j.Raw != "" {
			j.Raw = absolute(m.BaseDir, j.Raw)
		}
		if j.Project == "" {
			j.Project = "1"
		}
		m.Jobs[i] = j
	}
	return m, nil
}
func validateAssertions(assertions []Assertion) error {
	for _, assertion := range assertions {
		if assertion.Path != "" && !strings.HasPrefix(assertion.Path, "/") {
			return fmt.Errorf("assertion path must be a JSON pointer")
		}
		switch assertion.Op {
		case "eq", "ne", "exists", "absent", "contains", "not_contains":
		default:
			return fmt.Errorf("unknown assertion operator: %s", assertion.Op)
		}
	}
	return nil
}
func absolute(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}
func text(v any) string { s, _ := v.(string); return s }
func timestamp(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-0700"} {
		if t, e := time.Parse(layout, v); e == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp: %s", v)
}
