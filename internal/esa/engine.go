package esa

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"esa/internal/classifier"
)

var ErrInvalid = errors.New("invalid request")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

type Engine struct {
	IssueDirectory string
	Store          *Store
	Config         Config
	Now            func() time.Time
	classifiers    map[string]*classifier.Model
	workerID       string
	clock          atomic.Int64
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
		e.Config.Worker.MaxActivePerProject = EngineeringConcurrency + 1
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
	if err := e.update(func(*State) error { return nil }); err != nil {
		return nil, err
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
	t.StoredFields = nil
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
	err := e.update(func(s *State) error {
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
					c.State = "invalidated"
					c.ReviewNote = "The source ticket was reopened."
					c.Version++
					c.UpdatedAt = now
					s.Cases[c.ID] = c
					s.CaseHistory = append(s.CaseHistory, c)
				}
				receipt = e.enqueue(s, t, now, event.Force)
				receipt.EventID = event.ID
			}
			supersedeInactiveTask(s, previousRunID, now)
			touchTicketIssue(s, t.ID, now)
		}
		e.invalidateApplications(s)
		s.Events[event.ID] = event
		s.Receipts[event.ID] = receipt
		return nil
	})
	return receipt, err
}

func (e *Engine) collectCase(s *State, t Ticket, now time.Time) Case {
	c := Case{ID: "case_" + hash(t.ID)[:24], TicketID: t.ID, ProjectID: t.ProjectID, SourceKey: t.SourceKey, Subject: t.Subject, Summary: t.Description, Components: unique(t.Components), Versions: unique(t.Versions), Environment: t.Environment, Resolution: t.Resolution, State: "draft", CreatedAt: now, UpdatedAt: now, Version: 1}
	c.IssueID = s.Runs[s.Latest[t.ID]].IssueID
	if override := s.IssueOverrides[t.ID]; override != "" {
		c.IssueID = override
	}
	c.SourceHash = hash(struct {
		Input      string
		Resolution Resolution
	}{diagnosticHash(t), t.Resolution})
	if lower(t.Resolution.Reason) == "duplicate" {
		c.State = "duplicate"
	} else if contains([]string{"rejected", "cancelled", "won't fix", "wontfix", "not a problem", "invalid"}, t.Resolution.Reason) {
		c.State = "archived"
	}
	if old, ok := s.Cases[c.ID]; ok {
		if c.IssueID == "" {
			c.IssueID = old.IssueID
		}
		if old.SourceHash == c.SourceHash && old.State != "invalidated" {
			return old
		}
		c.CreatedAt = old.CreatedAt
		c.Version = old.Version + 1
	}
	if sol, ok := canonicalSolution(*s, c.IssueID, caseScope(c)); ok && sol.State == "trusted" && sameResolution(sol.Resolution, c.Resolution) {
		c.SolutionID = sol.ID
		c.State = "trusted"
		c.ReviewedBy = sol.Reviewer
		c.ReviewNote = sol.Note
		c.TrustedAt = sol.AvailableAt
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
	err := e.update(func(s *State) error {
		c, ok := s.Cases[id]
		original := c
		if !ok {
			return ErrNotFound
		}
		if c.IssueID != "" {
			if _, exists := s.Issues[c.IssueID]; !exists {
				return fmt.Errorf("%w: engineering issue was removed", ErrConflict)
			}
			touchIssue(s, c.IssueID, e.Now())
		}
		if review.Reviewer == "" || !contains([]string{"approve", "reject", "withdraw"}, review.Action) {
			return fmt.Errorf("%w: reviewer and valid review action required", ErrInvalid)
		}
		if review.Resolution != nil {
			c.Resolution = *review.Resolution
		}
		if review.Action == "approve" {
			t, ok := s.Tickets[c.TicketID]
			if !ok || !t.Closed || t.Private || c.State == "duplicate" || c.State == "archived" || c.State == "invalidated" {
				return fmt.Errorf("%w: source ticket is not eligible", ErrInvalid)
			}
			if c.Resolution.Cause == "" || c.Resolution.Solution == "" || c.Resolution.Validation == "" || len(c.Components) == 0 || len(c.Versions) == 0 {
				return fmt.Errorf("%w: confirmed cause, solution, verification and scope required", ErrInvalid)
			}
		}
		state := map[string]string{"approve": "trusted", "reject": "rejected", "withdraw": "withdrawn"}[review.Action]
		if sol, ok := canonicalSolution(*s, c.IssueID, caseScope(c)); ok && c.IssueID != "" {
			c.SolutionID = sol.ID
			s.Cases[id] = c
			if sol.State == state && sameResolution(sol.Resolution, c.Resolution) {
				e.syncCases(s, sol)
			} else {
				next := sol
				next.State = state
				next.Origin = "engineer"
				next.SourceTicketID = c.TicketID
				next.Resolution = c.Resolution
				next.Reviewer = review.Reviewer
				next.Note = review.Note
				e.publishSolution(s, s.Investigations[sol.InvestigationID], sol, next)
			}
			result = s.Cases[id]
			if hash(result) != hash(original) && result.Version == original.Version {
				result.Version++
				result.UpdatedAt = e.Now()
				s.Cases[id] = result
				s.CaseHistory = append(s.CaseHistory, result)
			}
			return nil
		}
		if c.State == state && sameResolution(s.Cases[id].Resolution, c.Resolution) {
			result = c
			return nil
		}
		c.State = state
		c.ReviewedBy = review.Reviewer
		c.ReviewNote = review.Note
		c.Version++
		c.UpdatedAt = e.Now()
		if state == "trusted" {
			c.TrustedAt = e.Now()
		} else {
			c.TrustedAt = time.Time{}
		}
		s.Cases[id] = c
		s.CaseHistory = append(s.CaseHistory, c)
		e.invalidateApplications(s)
		result = c
		return nil
	})
	return result, err
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
