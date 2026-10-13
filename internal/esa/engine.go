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
	"sync/atomic"
	"time"

	"esa/internal/classifier"
)

var ErrInvalid = errors.New("invalid request")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

var words = regexp.MustCompile(`[A-Za-z0-9_]+(?:[.][A-Za-z0-9_]+)*`)
var exceptions = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9_.]*(?:Exception|Error)\b`)
var stopWords = map[string]bool{"the": true, "this": true, "that": true, "with": true, "from": true, "have": true, "when": true, "then": true, "there": true, "issue": true, "error": true, "exception": true, "timeout": true, "oom": true, "using": true, "after": true, "before": true, "does": true, "should": true, "could": true, "would": true, "and": true, "for": true, "not": true, "are": true, "was": true, "can": true, "but": true, "our": true, "has": true, "will": true, "how": true}

type Engine struct {
	Store       *Store
	Config      Config
	Now         func() time.Time
	classifiers map[string]*classifier.Model
	workerID    string
	clock       atomic.Int64
}

func NewEngine(store *Store, config Config) (*Engine, error) {
	if config.Version == "" || config.Budget.MaxCalls < 0 || config.Budget.MaxCalls > 1 || config.Budget.TimeoutSeconds < 1 || config.Budget.MaxInputTokens < 1 || config.Budget.MaxOutputTokens < 1 {
		return nil, fmt.Errorf("%w: invalid workflow configuration or budget", ErrInvalid)
	}
	e := &Engine{Store: store, Config: config, Now: func() time.Time { return time.Now().UTC() }, classifiers: map[string]*classifier.Model{}}
	if config.Classifier.Enabled {
		if len(config.Classifier.Models) == 0 {
			return nil, fmt.Errorf("%w: enabled classifier requires project models", ErrInvalid)
		}
		for project, path := range config.Classifier.Models {
			m, err := classifier.Load(path)
			if err != nil {
				return nil, fmt.Errorf("classifier for project %s: %w", project, err)
			}
			if m.ProjectID != project {
				return nil, fmt.Errorf("%w: classifier project mismatch", ErrInvalid)
			}
			e.classifiers[project] = m
		}
	}
	if config.Worker.MaxActivePerProject == 0 {
		e.Config.Worker.MaxActivePerProject = 1
	}
	if e.Config.Worker.MaxActivePerProject < 1 || e.Config.Worker.MaxActivePerProject > 8 {
		return nil, fmt.Errorf("%w: max_active_per_project must be 1..8", ErrInvalid)
	}
	if config.Worker.LeaseSeconds == 0 {
		e.Config.Worker.LeaseSeconds = 60
	}
	if e.Config.Worker.LeaseSeconds < 3 || e.Config.Worker.LeaseSeconds > 3600 {
		return nil, fmt.Errorf("%w: lease must be 3..3600 seconds", ErrInvalid)
	}
	e.workerID = randomID("worker_")
	if config.Replay.Enabled {
		if config.Replay.InitialTime.IsZero() || len(config.Replay.Projects) == 0 {
			return nil, fmt.Errorf("%w: replay clock requires initial_time and clock_projects", ErrInvalid)
		}
		at := store.Snapshot().ReplayClock
		if at.IsZero() {
			at = config.Replay.InitialTime
		}
		e.clock.Store(at.UnixNano())
		e.Now = func() time.Time { return time.Unix(0, e.clock.Load()).UTC() }
	}
	return e, nil
}

func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

func (e *Engine) Refresh() error {
	if err := e.Store.Refresh(); err != nil {
		return err
	}
	if e.Config.Replay.Enabled {
		e.Store.mu.RLock()
		at := e.Store.state.ReplayClock
		e.Store.mu.RUnlock()
		for !at.IsZero() {
			old := e.clock.Load()
			if at.UnixNano() <= old || e.clock.CompareAndSwap(old, at.UnixNano()) {
				break
			}
		}
	}
	return nil
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

func matchCases(t Ticket, cases []Case, limit int) []Match {
	out := []Match{}
	kw := keywords(ticketText(t))
	sig := signals(t)
	for _, c := range cases {
		if c.State != "trusted" || c.ProjectID != t.ProjectID || c.TicketID == t.ID {
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
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (e *Engine) Decide(t Ticket, history []Case, at time.Time) ([]Match, string, string, []string) {
	if t.Private {
		return nil, "restricted", "restricted", nil
	}
	if t.ForwardedBy == "" || t.ForwardedAt.IsZero() || t.ForwardedAt.After(at) {
		return nil, "manual_forward", "awaiting_forward", nil
	}
	matches := MatchCases(t, CasesAt(history, at))
	for _, match := range matches {
		if match.Applicable {
			return matches, "case_reuse", "completed", nil
		}
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
		return matches, "request_information", "awaiting_information", missing
	}
	return matches, "engineering_analysis", "queued", nil
}
func diagnosticHash(t Ticket) string {
	comments := []string{}
	for _, c := range t.Comments {
		comments = append(comments, c.Body)
	}
	return hash(struct {
		Subject, Description, Tracker          string
		Components, Versions, Errors, Comments []string
		Environment                            map[string]string
	}{t.Subject, t.Description, t.Tracker, unique(t.Components), unique(t.Versions), unique(t.ErrorCodes), comments, t.Environment})
}

func (e *Engine) Receive(event Event) (Receipt, error) {
	t := event.Ticket
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.ProjectID) == "" || strings.TrimSpace(t.Subject) == "" {
		return Receipt{}, fmt.Errorf("%w: event ID, ticket ID, project ID, and subject are required", ErrInvalid)
	}
	if strings.TrimSpace(event.ForwardedBy) == "" || event.ForwardedAt.IsZero() || event.ForwardedAt.After(e.Now()) {
		return Receipt{}, fmt.Errorf("%w: explicit manual forwarding identity and time required", ErrInvalid)
	}
	t.ForwardedBy = event.ForwardedBy
	t.ForwardedAt = event.ForwardedAt
	t.CategoryOverride = ""
	t.Labels = nil
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
	if t.UpdatedAt.After(event.ForwardedAt) || t.CreatedAt.After(event.ForwardedAt) {
		return Receipt{}, fmt.Errorf("%w: snapshot source time exceeds forwarding cutoff", ErrInvalid)
	}
	for _, comment := range t.Comments {
		if comment.CreatedAt.After(event.ForwardedAt) || comment.UpdatedAt.After(event.ForwardedAt) || e.Config.Replay.Enabled && comment.CreatedAt.IsZero() {
			return Receipt{}, fmt.Errorf("%w: comment provenance is outside the observation cutoff", ErrInvalid)
		}
	}
	event.Ticket = t
	var receipt Receipt
	err := e.Store.Update(func(s *State) error {
		generation := s.Generation
		previousRunID := s.Latest[t.ID]
		if old, ok := s.Events[event.ID]; ok {
			if hash(old.Ticket) != hash(t) || old.Origin != event.Origin || old.Force != event.Force {
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
			if previous, ok := s.Tickets[t.ID]; ok && ((previous.Closed && !t.Closed) || investigationScope(previous) != investigationScope(t)) {
				e.withdrawSourceSolutions(s, t.ID, "Source was reopened or its applicability scope changed")
			}
			s.Tickets[t.ID] = t
			captureSnapshot(s, event)
			e.detachChangedMemberships(s, t)
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
				if r, ok := s.Runs[s.Latest[t.ID]]; ok && r.InvestigationID == "" {
					r.State = "restricted"
					s.Runs[r.ID] = r
				}
				receipt.Action = "restricted_private_ticket"
			} else if t.Closed {
				c := e.collectCase(s, t, now)
				receipt.Action = "case_collected"
				receipt.CaseID = c.ID
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
				receipt = e.enqueue(s, t, now, event.Force)
				receipt.EventID = event.ID
			}
			supersedeInactiveTask(s, previousRunID, now)
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
	c := Case{ID: "case_" + hash(t.ID)[:24], TicketID: t.ID, ProjectID: t.ProjectID, SourceKey: t.SourceKey, Subject: t.Subject, Summary: t.Description, Components: unique(t.Components), Versions: unique(t.Versions), Environment: t.Environment, Signals: signals(t), Keywords: keywords(ticketText(t)), Resolution: t.Resolution, State: "draft", CreatedAt: now, UpdatedAt: now, Version: 1}
	c.IssueID = s.Runs[s.Latest[t.ID]].IssueID
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
			if c.Resolution.Cause == "" || c.Resolution.Solution == "" || c.Resolution.Validation == "" || len(c.Components) == 0 || len(c.Versions) == 0 {
				return fmt.Errorf("%w: confirmed cause, solution, validation, component, and affected version are required", ErrInvalid)
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
			e.publishReviewedCase(s, c)
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
		if t.Closed || t.ForwardedBy == "" || t.ForwardedAt.IsZero() {
			continue
		}
		matches, route, status, missing := e.Decide(t, eligibleCaseHistory(*s, t), now)
		old := s.Runs[s.Latest[t.ID]]
		if route == "case_reuse" {
			if old.Route == route && old.State == status && old.CaseGeneration == s.Generation && old.RuleVersion == e.Config.Version && old.InputHash == diagnosticHash(t) {
				continue
			}
			// This is a later recommendation observation. Preserve the forwarded
			// snapshot and initial diagnosis cutoff; never enqueue analysis here.
			run := e.newRun(s, t, s.Snapshots[s.LatestSnapshot[t.ID]], now, now)
			run.Route, run.State, run.Matches, run.MissingInformation = route, status, matches, missing
			recordRun(s, run)
			supersedeInactiveTask(s, old.ID, now)
		} else if old.Route == "case_reuse" && old.State != "invalidated" {
			old.State = "invalidated"
			old.Error = "A recommended case changed or was withdrawn. Reassess this ticket before applying its earlier recommendation."
			old.UpdatedAt = now
			s.Runs[old.ID] = old
		}
	}
}

func (e *Engine) AnalyzeTicket(id string, force bool, actor string) (Receipt, error) {
	ticket, ok := e.Store.Snapshot().Tickets[id]
	if !ok || ticket.ForwardedBy == "" {
		return Receipt{}, ErrNotFound
	}
	if ticket.Closed {
		return Receipt{}, fmt.Errorf("%w: forward the latest closed snapshot to collect a case", ErrInvalid)
	}
	return e.Receive(Event{ID: randomID("manual_"), Origin: "manual_forward", ForwardedBy: actor, ForwardedAt: e.Now(), OccurredAt: e.Now(), Force: force, Ticket: ticket})
}
