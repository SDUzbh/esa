package esa

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid request")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

var words = regexp.MustCompile(`[A-Za-z0-9_]+(?:[.][A-Za-z0-9_]+)*`)
var exceptions = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9_.]*(?:Exception|Error)\b`)
var stopWords = map[string]bool{"the": true, "this": true, "that": true, "with": true, "from": true, "have": true, "when": true, "then": true, "there": true, "issue": true, "error": true, "exception": true, "timeout": true, "oom": true, "using": true, "after": true, "before": true, "does": true, "should": true, "could": true, "would": true, "and": true, "for": true, "not": true, "are": true, "was": true, "can": true, "but": true, "our": true, "has": true, "will": true, "how": true}

type Engine struct {
	Store  *Store
	Config Config
	Now    func() time.Time
}

func NewEngine(store *Store, config Config) (*Engine, error) {
	if config.Version == "" || len(config.Rules) == 0 || config.Budget.MaxCalls < 0 || config.Budget.MaxCalls > 1 || config.Budget.TimeoutSeconds < 1 || config.Budget.MaxInputTokens < 1 || config.Budget.MaxOutputTokens < 1 {
		return nil, fmt.Errorf("%w: invalid rules or budget", ErrInvalid)
	}
	ids := map[string]bool{}
	for _, r := range config.Rules {
		if r.ID == "" || r.Category == "" || ids[r.ID] || len(r.Any)+len(r.All)+len(r.Trackers) == 0 {
			return nil, fmt.Errorf("%w: invalid or duplicate rule", ErrInvalid)
		}
		ids[r.ID] = true
	}
	return &Engine{Store: store, Config: config, Now: func() time.Time { return time.Now().UTC() }}, nil
}

func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if lower(x) == lower(s) {
			return true
		}
	}
	return false
}
func unique(xs []string) []string {
	set := map[string]bool{}
	for _, x := range xs {
		if x = lower(x); x != "" {
			set[x] = true
		}
	}
	out := []string{}
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
func intersects(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if contains(b, x) {
			out = append(out, x)
		}
	}
	return unique(out)
}
func ticketText(t Ticket) string {
	var text strings.Builder
	text.WriteString(t.Subject)
	text.WriteByte('\n')
	text.WriteString(t.Description)
	for _, c := range t.Comments {
		text.WriteByte('\n')
		text.WriteString(c.Body)
	}
	return text.String()
}
func keywords(text string) []string {
	out := []string{}
	for _, w := range words.FindAllString(lower(text), -1) {
		if len(w) > 2 && !stopWords[w] {
			out = append(out, w)
		}
	}
	return unique(out)
}
func signals(t Ticket) []string {
	out := append([]string{}, t.ErrorCodes...)
	out = append(out, exceptions.FindAllString(ticketText(t), -1)...)
	filtered := []string{}
	for _, x := range unique(out) {
		if !stopWords[x] && x != "timeoutexception" && x != "java.util.concurrent.timeoutexception" {
			filtered = append(filtered, x)
		}
	}
	return filtered
}
func hasPhrase(text, phrase string) bool {
	phrase = lower(phrase)
	if phrase == "" {
		return false
	}
	text = strings.ToLower(text)
	word := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' }
	for offset := 0; offset <= len(text)-len(phrase); {
		i := strings.Index(text[offset:], phrase)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(phrase)
		if (i == 0 || !word(text[i-1])) && (end == len(text) || !word(text[end])) {
			return true
		}
		offset = i + 1
	}
	return false
}

func (e *Engine) Classify(t Ticket) Classification {
	if t.CategoryOverride != "" {
		return Classification{Category: t.CategoryOverride, RuleIDs: []string{"human_override"}}
	}
	text := strings.ToLower(ticketText(t))
	out := Classification{Category: "unknown", RuleIDs: []string{}}
	categories := []string{}
	for _, r := range e.Config.Rules {
		if len(r.Trackers) > 0 && !contains(r.Trackers, t.Tracker) {
			continue
		}
		blocked := false
		for _, p := range r.None {
			if hasPhrase(text, p) {
				blocked = true
			}
		}
		if blocked {
			continue
		}
		all := true
		for _, p := range r.All {
			if !hasPhrase(text, p) {
				all = false
			}
		}
		if !all {
			continue
		}
		any := len(r.Any) == 0
		for _, p := range r.Any {
			if hasPhrase(text, p) {
				any = true
			}
		}
		if !any {
			continue
		}
		categories = append(categories, r.Category)
		out.RuleIDs = append(out.RuleIDs, r.ID)
	}
	categories = unique(categories)
	if len(categories) == 1 {
		out.Category = categories[0]
	} else if len(categories) > 1 {
		out.Candidates = categories
	}
	return out
}

// CasesAt selects the latest case revision visible at the observation time.
func CasesAt(history []Case, at time.Time) []Case {
	latest := map[string]Case{}
	for _, c := range history {
		if c.CreatedAt.After(at) || c.UpdatedAt.After(at) {
			continue
		}
		old, ok := latest[c.ID]
		if !ok || c.Version > old.Version {
			latest[c.ID] = c
		}
	}
	out := []Case{}
	for _, c := range latest {
		if c.State == "trusted" && !c.TrustedAt.IsZero() && !c.TrustedAt.After(at) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func MatchCases(t Ticket, category string, cases []Case) []Match {
	out := []Match{}
	kw := keywords(ticketText(t))
	sig := signals(t)
	for _, c := range cases {
		if c.State != "trusted" || c.ProjectID != t.ProjectID || c.TicketID == t.ID {
			continue
		}
		if category != "unknown" && c.Category != category {
			continue
		}
		commonWords := intersects(kw, c.Keywords)
		commonSignals := intersects(sig, c.Signals)
		commonComponents := intersects(t.Components, c.Components)
		commonVersions := intersects(t.Versions, c.Versions)
		if len(commonWords) < 2 && len(commonSignals) == 0 {
			continue
		}
		m := Match{CaseID: c.ID, CaseVersion: c.Version, Subject: c.Subject, Reasons: []string{}, Score: len(commonWords)}
		if len(commonSignals) > 0 {
			m.Score += 10
			m.Reasons = append(m.Reasons, "Matching diagnostic signals: "+strings.Join(commonSignals, ", "))
		}
		if len(commonWords) > 0 {
			m.Reasons = append(m.Reasons, "Matching keywords: "+strings.Join(commonWords, ", "))
		}
		if len(commonComponents) > 0 {
			m.Score += 5
			m.Reasons = append(m.Reasons, "Matching component")
		}
		if len(commonVersions) > 0 {
			m.Score += 5
			m.Reasons = append(m.Reasons, "Matching affected version")
		}
		environmentFits := true
		for k, v := range c.Environment {
			if v != "" && lower(t.Environment[k]) != lower(v) {
				environmentFits = false
			}
		}
		m.Applicable = len(commonComponents) > 0 && len(commonVersions) > 0 && environmentFits && (len(commonSignals) > 0 || len(commonWords) >= 4)
		if m.Applicable {
			m.Solution = c.Resolution.Solution
		} else {
			m.Reasons = append(m.Reasons, "Applicability requires further verification")
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Applicable != out[j].Applicable {
			return out[i].Applicable
		}
		if out[i].Score == out[j].Score {
			return out[i].CaseID < out[j].CaseID
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func (e *Engine) Decide(t Ticket, history []Case, at time.Time) (Classification, []Match, string, string, []string) {
	c := e.Classify(t)
	if t.Private {
		return c, nil, "restricted", "restricted", nil
	}
	matches := MatchCases(t, c.Category, CasesAt(history, at))
	for _, m := range matches {
		if m.Applicable {
			return c, matches, "case_reuse", "completed", nil
		}
	}
	if c.Category == "unknown" {
		return c, matches, "request_information", "awaiting_information", []string{"Confirm the issue category and provide a specific symptom or diagnostic signal."}
	}
	if !contains(e.Config.EngineeringCategories, c.Category) {
		return c, matches, "customer_support", "completed", nil
	}
	missing := []string{}
	if len(t.Components) == 0 {
		missing = append(missing, "Affected component")
	}
	if len(t.Versions) == 0 {
		missing = append(missing, "Affected version")
	}
	if len(t.Environment) == 0 {
		missing = append(missing, "Runtime environment")
	}
	if strings.TrimSpace(t.Description) == "" {
		missing = append(missing, "Observed behavior and reproduction conditions")
	}
	if len(missing) > 0 {
		return c, matches, "request_information", "awaiting_information", missing
	}
	return c, matches, "engineering_analysis", "queued", nil
}

func diagnosticHash(t Ticket) string {
	comments := []string{}
	for _, c := range t.Comments {
		comments = append(comments, c.Body)
	}
	return hash(struct {
		Subject, Description, Tracker, Override string
		Components, Versions, Errors, Comments  []string
		Environment                             map[string]string
	}{t.Subject, t.Description, t.Tracker, t.CategoryOverride, unique(t.Components), unique(t.Versions), unique(t.ErrorCodes), comments, t.Environment})
}

func (e *Engine) enqueue(s *State, t Ticket, now time.Time, force bool) Receipt {
	h := diagnosticHash(t)
	if previous, ok := s.Runs[s.Latest[t.ID]]; ok && !force && previous.InputHash == h && previous.RuleVersion == e.Config.Version && previous.CaseGeneration == s.Generation && previous.WorkflowVersion == WorkflowVersion {
		return Receipt{TicketID: t.ID, RunID: previous.ID, Action: "cached"}
	}
	c, matches, route, status, missing := e.Decide(t, s.CaseHistory, now)
	r := Run{ID: randomID("run_"), TicketID: t.ID, ProjectID: t.ProjectID, InputHash: h, WorkflowVersion: WorkflowVersion, RuleVersion: e.Config.Version, CaseGeneration: s.Generation, Input: t, Classification: c, Route: route, State: status, Matches: matches, MissingInformation: missing, CreatedAt: now, UpdatedAt: now}
	if route == "case_reuse" {
		r.Report.Summary = "Applicable resolved cases were found. Review their evidence and verify the proposed solution."
		for _, m := range matches {
			if m.Applicable {
				r.Report.Recommendations = append(r.Report.Recommendations, m.Solution)
				r.Report.Evidence = append(r.Report.Evidence, Evidence{Source: "resolved_case", Reference: fmt.Sprintf("%s@%d", m.CaseID, m.CaseVersion), Quote: m.Solution})
			}
		}
	}
	if route == "customer_support" {
		r.Report.Summary = "This category is routed to customer support without an engineering Agent call."
	}
	s.Runs[r.ID] = r
	s.Latest[t.ID] = r.ID
	return Receipt{TicketID: t.ID, RunID: r.ID, Action: "created"}
}

func (e *Engine) Receive(event Event) (Receipt, error) {
	t := event.Ticket
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.ProjectID) == "" || strings.TrimSpace(t.Subject) == "" {
		return Receipt{}, fmt.Errorf("%w: event ID, ticket ID, project ID, and subject are required", ErrInvalid)
	}
	if t.CategoryOverride != "" {
		known := t.CategoryOverride == "unknown"
		for _, r := range e.Config.Rules {
			if r.Category == t.CategoryOverride {
				known = true
			}
		}
		if !known {
			return Receipt{}, fmt.Errorf("%w: unknown category override", ErrInvalid)
		}
	}
	now := e.Now()
	event.ReceivedAt = now
	if event.OccurredAt.IsZero() {
		event.OccurredAt = t.UpdatedAt
	}
	if event.OccurredAt.IsZero() {
		return Receipt{}, fmt.Errorf("%w: source timestamp is required", ErrInvalid)
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = event.OccurredAt
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = t.UpdatedAt
	}
	event.Ticket = t
	var receipt Receipt
	err := e.Store.Update(func(s *State) error {
		generation := s.Generation
		if old, ok := s.Events[event.ID]; ok {
			if hash(old.Ticket) != hash(t) || old.Origin != event.Origin {
				return fmt.Errorf("%w: event ID was reused with different content", ErrConflict)
			}
			receipt = s.Receipts[event.ID]
			receipt.Action = "duplicate"
			return nil
		}
		if old, ok := s.Tickets[t.ID]; ok && old.ProjectID != t.ProjectID {
			return fmt.Errorf("%w: ticket belongs to a different project", ErrConflict)
		}
		receipt = Receipt{EventID: event.ID, TicketID: t.ID}
		if event.Origin == "esa" {
			receipt.Action = "ignored_self_event"
		} else if old, ok := s.Tickets[t.ID]; ok && t.UpdatedAt.Before(old.UpdatedAt) {
			receipt.Action = "ignored_stale_event"
		} else {
			s.Tickets[t.ID] = t
			if t.Private {
				if c, ok := s.Cases["case_"+hash(t.ID)[:24]]; ok && c.State != "invalidated" {
					if c.State == "trusted" {
						s.Generation++
					}
					c.State = "invalidated"
					c.Version++
					c.UpdatedAt = now
					c.ReviewNote = "The source ticket became private."
					s.Cases[c.ID] = c
					s.CaseHistory = append(s.CaseHistory, c)
				}
				if r, ok := s.Runs[s.Latest[t.ID]]; ok {
					r.State = "restricted"
					s.Runs[r.ID] = r
				}
				receipt.Action = "restricted_private_ticket"
			} else if t.Closed {
				c := e.collectCase(s, t, now)
				receipt.Action = "case_collected"
				receipt.CaseID = c.ID
				if id := s.Latest[t.ID]; id != "" {
					r := s.Runs[id]
					if r.State == "queued" || r.State == "running" {
						r.State = "superseded"
						r.UpdatedAt = now
						s.Runs[id] = r
					}
				}
			} else {
				if c, ok := s.Cases["case_"+hash(t.ID)[:24]]; ok && c.State != "invalidated" {
					wasTrusted := c.State == "trusted"
					c.State = "invalidated"
					c.ReviewNote = "The source ticket was reopened."
					c.Version++
					c.UpdatedAt = now
					s.Cases[c.ID] = c
					s.CaseHistory = append(s.CaseHistory, c)
					if wasTrusted {
						s.Generation++
					}
				}
				receipt = e.enqueue(s, t, now, false)
				receipt.EventID = event.ID
			}
		}
		s.Events[event.ID] = event
		s.Receipts[event.ID] = receipt
		if generation != s.Generation {
			e.refreshRecommendations(s, now)
		}
		return nil
	})
	return receipt, err
}

func (e *Engine) collectCase(s *State, t Ticket, now time.Time) Case {
	c := Case{ID: "case_" + hash(t.ID)[:24], TicketID: t.ID, ProjectID: t.ProjectID, SourceKey: t.SourceKey, Subject: t.Subject, Summary: t.Description, Category: e.Classify(t).Category, Components: unique(t.Components), Versions: unique(t.Versions), Environment: t.Environment, Signals: signals(t), Keywords: keywords(ticketText(t)), Resolution: t.Resolution, State: "draft", CreatedAt: now, UpdatedAt: now, Version: 1}
	c.SourceHash = hash(struct {
		Input      string
		Resolution Resolution
	}{diagnosticHash(t), t.Resolution})
	if len(c.Keywords) > 100 {
		c.Keywords = c.Keywords[:100]
	}
	if lower(t.Resolution.Reason) == "duplicate" {
		c.State = "duplicate"
	} else if contains([]string{"rejected", "cancelled", "won't fix", "wontfix", "not a problem", "invalid"}, t.Resolution.Reason) {
		c.State = "archived"
	}
	if old, ok := s.Cases[c.ID]; ok {
		if old.SourceHash == c.SourceHash && old.State != "invalidated" {
			return old
		}
		c.CreatedAt = old.CreatedAt
		c.Version = old.Version + 1
		if old.State == "trusted" {
			s.Generation++
		}
	}
	s.Cases[c.ID] = c
	s.CaseHistory = append(s.CaseHistory, c)
	return c
}

type Review struct {
	Action     string      `json:"action"`
	Reviewer   string      `json:"reviewer"`
	Note       string      `json:"note"`
	Resolution *Resolution `json:"resolution,omitempty"`
}

func (e *Engine) ReviewCase(id string, review Review) (Case, error) {
	var result Case
	err := e.Store.Update(func(s *State) error {
		c, ok := s.Cases[id]
		if !ok {
			return ErrNotFound
		}
		if review.Reviewer == "" || !contains([]string{"approve", "reject"}, review.Action) {
			return fmt.Errorf("%w: reviewer and approve/reject action are required", ErrInvalid)
		}
		if review.Resolution != nil {
			c.Resolution = *review.Resolution
		}
		wasTrusted := c.State == "trusted"
		if review.Action == "approve" {
			t, ok := s.Tickets[c.TicketID]
			if !ok || !t.Closed || t.Private || c.State == "duplicate" || c.State == "archived" || c.State == "invalidated" {
				return fmt.Errorf("%w: source ticket is not eligible for a trusted case", ErrInvalid)
			}
			if c.Resolution.Cause == "" || c.Resolution.Solution == "" || c.Resolution.Validation == "" || len(c.Components) == 0 || len(c.Versions) == 0 || c.Category == "unknown" {
				return fmt.Errorf("%w: confirmed cause, solution, validation, category, component, and affected version are required", ErrInvalid)
			}
			c.State = "trusted"
			c.TrustedAt = e.Now()
		} else {
			c.State = "rejected"
			c.TrustedAt = time.Time{}
		}
		c.ReviewedBy = review.Reviewer
		c.ReviewNote = review.Note
		c.Version++
		c.UpdatedAt = e.Now()
		s.Cases[id] = c
		s.CaseHistory = append(s.CaseHistory, c)
		if wasTrusted || c.State == "trusted" {
			s.Generation++
			e.refreshRecommendations(s, e.Now())
		}
		result = c
		return nil
	})
	return result, err
}

// Refresh only recommendations. Learning a case never triggers a new model call.
func (e *Engine) refreshRecommendations(s *State, now time.Time) {
	for _, t := range s.Tickets {
		if t.Closed {
			continue
		}
		_, _, route, _, _ := e.Decide(t, s.CaseHistory, now)
		old := s.Runs[s.Latest[t.ID]]
		if route == "case_reuse" {
			if old.State == "queued" || old.State == "running" {
				old.State = "superseded"
				old.UpdatedAt = now
				s.Runs[old.ID] = old
			}
			e.enqueue(s, t, now, false)
		} else if old.Route == "case_reuse" && old.State != "invalidated" {
			old.State = "invalidated"
			old.Error = "A recommended case changed or was withdrawn. Reassess this ticket before applying its earlier recommendation."
			old.UpdatedAt = now
			s.Runs[old.ID] = old
		}
	}
}

func (e *Engine) AnalyzeTicket(id string, force bool) (Receipt, error) {
	var receipt Receipt
	err := e.Store.Update(func(s *State) error {
		t, ok := s.Tickets[id]
		if !ok {
			return ErrNotFound
		}
		if t.Closed {
			return fmt.Errorf("%w: closed tickets are collected as cases", ErrInvalid)
		}
		receipt = e.enqueue(s, t, e.Now(), force)
		return nil
	})
	return receipt, err
}
