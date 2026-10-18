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
	"sync"
	"sync/atomic"
	"time"

	"esa/internal/classifier"
)

var ErrInvalid = errors.New("invalid request")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

// ErrRejected marks an event that can never be accepted as sent. Retrying it
// cannot succeed, unlike store, contention or clock-skew failures.
var ErrRejected = errors.New("event rejected")

// rejectedError keeps the wrapped message and status sentinel for HTTP callers.
type rejectedError struct{ error }

func (r rejectedError) Unwrap() error        { return r.error }
func (r rejectedError) Is(target error) bool { return target == ErrRejected }
func reject(err error) error                 { return rejectedError{err} }

// Engine coordinates one business workflow over the selected authoritative Store.
// Provider calls run outside state transactions; mutations use validated snapshots.
type Engine struct {
	IssueDirectory string
	Store          *Store
	Config         Config
	Now            func() time.Time
	classifiers    map[string]*classifier.Model
	workerID       string
	clock          atomic.Int64
	tempRootOnce   sync.Once
	tempRoot       string
	tempRootErr    error
}

// WorkSignal closes when a worker or an engineering slot frees in this process.
func (e *Engine) WorkSignal() <-chan struct{} { return e.Store.WorkSignal() }

// NewEngine validates budgets, worker limits and optional project models.
// It initializes replay time only when explicitly configured and enforces Issue retention.
func NewEngine(store *Store, config Config) (*Engine, error) {
	e, err := configuredEngine(config)
	if err != nil {
		return nil, err
	}
	e.Store = store
	if config.Replay.Enabled {
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

// ValidateConfig reports whether NewEngine would accept the configuration,
// loading any classifier models, without a store.
func ValidateConfig(config Config) error {
	_, err := configuredEngine(config)
	return err
}

// configuredEngine validates the configuration, applies its defaults and
// loads classifier models; NewEngine then binds the store.
func configuredEngine(config Config) (*Engine, error) {
	if config.Version == "" ||
		config.Budget.MaxCalls < 0 ||
		config.Budget.MaxCalls > 1 ||
		config.Budget.TimeoutSeconds < 1 ||
		config.Budget.MaxInputTokens < 1 ||
		config.Budget.MaxOutputTokens < 1 ||
		!config.Budget.validWeights() {
		return nil, fmt.Errorf("%w: invalid workflow configuration or budget", ErrInvalid)
	}
	e := &Engine{Config: config, Now: func() time.Time { return time.Now().UTC() }, classifiers: map[string]*classifier.Model{}}
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
	if config.Worker.MaxQueueWaitSeconds == 0 {
		e.Config.Worker.MaxQueueWaitSeconds = 1800
	}
	if e.Config.Worker.MaxQueueWaitSeconds < 1 || e.Config.Worker.MaxQueueWaitSeconds > 86400 {
		return nil, fmt.Errorf("%w: max_queue_wait_seconds must be 1..86400", ErrInvalid)
	}
	if config.IssueRetirementDays == 0 {
		e.Config.IssueRetirementDays = 180
	}
	if e.Config.IssueRetirementDays < 1 || e.Config.IssueRetirementDays > 3650 {
		return nil, fmt.Errorf("%w: issue_retirement_days must be 1..3650", ErrInvalid)
	}
	if config.Replay.Enabled && (config.Replay.InitialTime.IsZero() || len(config.Replay.Projects) == 0) {
		return nil, fmt.Errorf("%w: replay clock requires initial_time and clock_projects", ErrInvalid)
	}
	e.workerID = randomID("worker_")
	return e, nil
}

func randomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

// Refresh advances the replay test clock to the stored replay time.
func (e *Engine) Refresh() error {
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
		if c.State == CaseTrusted && !c.TrustedAt.IsZero() && !c.TrustedAt.After(at) {
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

// Receive validates manual forwarding, timestamps and event idempotency before persistence.
// Private tickets remain restricted; closed tickets collect cases; open tickets queue/share work.
// A rejected or stale event cannot overwrite a newer accepted diagnostic snapshot.
func (e *Engine) Receive(event Event) (Receipt, error) {
	t := event.Ticket
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.ProjectID) == "" || strings.TrimSpace(t.Subject) == "" {
		return Receipt{}, reject(fmt.Errorf("%w: event ID, ticket ID, project ID, and subject are required", ErrInvalid))
	}
	if strings.TrimSpace(event.ForwardedBy) == "" || event.ForwardedAt.IsZero() {
		return Receipt{}, reject(fmt.Errorf("%w: explicit manual forwarding identity and time required", ErrInvalid))
	}
	// A forwarding time ahead of this clock is skew, not a defective event: retry it.
	if event.ForwardedAt.After(e.Now()) {
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
		return Receipt{}, reject(fmt.Errorf("%w: source timestamp is required", ErrInvalid))
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = event.OccurredAt
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = t.UpdatedAt
	}
	if t.UpdatedAt.After(event.ForwardedAt) || t.CreatedAt.After(event.ForwardedAt) {
		return Receipt{}, reject(fmt.Errorf("%w: snapshot source time exceeds forwarding cutoff", ErrInvalid))
	}
	for _, comment := range t.Comments {
		if comment.CreatedAt.After(event.ForwardedAt) || comment.UpdatedAt.After(event.ForwardedAt) || e.Config.Replay.Enabled && comment.CreatedAt.IsZero() {
			return Receipt{}, reject(fmt.Errorf("%w: comment provenance is outside the observation cutoff", ErrInvalid))
		}
	}
	event.Ticket = t
	ticketHash := hash(t)
	var receipt Receipt
	err := e.update(func(s *State) error {
		previousRunID := s.Latest.Get(t.ID)
		record := EventRecord{
			ID:          event.ID,
			Origin:      event.Origin,
			Force:       event.Force,
			ForwardedBy: event.ForwardedBy,
			ForwardedAt: event.ForwardedAt,
			OccurredAt:  event.OccurredAt,
			ReceivedAt:  event.ReceivedAt,
			TicketID:    t.ID,
			TicketHash:  ticketHash,
			Raw:         event.Raw,
		}
		if old, ok := s.Events.Lookup(event.ID); ok {
			if old.TicketHash != ticketHash || old.Origin != event.Origin || old.Force != event.Force {
				return reject(fmt.Errorf("%w: event ID was reused with different content", ErrConflict))
			}
			receipt = s.Receipts.Get(event.ID)
			receipt.Action = "duplicate"
			return nil
		}
		if old, ok := s.Tickets.Lookup(t.ID); ok && old.ProjectID != t.ProjectID {
			return reject(fmt.Errorf("%w: ticket belongs to a different project", ErrConflict))
		}
		receipt = Receipt{EventID: event.ID, TicketID: t.ID}
		if event.Origin == "esa" {
			receipt.Action = "ignored_self_event"
		} else if old, ok := s.Tickets.Lookup(t.ID); ok && t.UpdatedAt.Before(old.UpdatedAt) {
			receipt.Action = "ignored_stale_event"
		} else {
			if previous, ok := s.Tickets.Lookup(t.ID); ok && ((previous.Closed && !t.Closed) || investigationScope(previous) != investigationScope(t)) {
				e.withdrawSourceSolutions(s, t.ID, "Source was reopened or its applicability scope changed")
			}
			s.Tickets.Put(t.ID, t)
			record.SnapshotID = captureSnapshot(s, event)
			e.detachChangedMemberships(s, t)
			if t.Private {
				if c, ok := s.Cases.Lookup("case_" + hash(t.ID)[:24]); ok && c.State != CaseInvalidated {
					c.State = CaseInvalidated
					c.Version++
					c.UpdatedAt = now
					c.ReviewNote = "The source ticket became private."
					s.Cases.Put(c.ID, c)
					s.CaseHistory = append(s.CaseHistory, c)
				}
				if r, ok := s.Runs.Lookup(s.Latest.Get(t.ID)); ok && r.InvestigationID == "" {
					r.State = RunRestricted
					s.Runs.Put(r.ID, r)
				}
				receipt.Action = "restricted_private_ticket"
			} else if t.Closed {
				c := e.collectCase(s, t, now)
				receipt.Action = "case_collected"
				receipt.CaseID = c.ID
			} else {
				if c, ok := s.Cases.Lookup("case_" + hash(t.ID)[:24]); ok && c.State != CaseInvalidated {
					c.State = CaseInvalidated
					c.ReviewNote = "The source ticket was reopened."
					c.Version++
					c.UpdatedAt = now
					s.Cases.Put(c.ID, c)
					s.CaseHistory = append(s.CaseHistory, c)
				}
				receipt = e.enqueue(s, t, now, event.Force)
				receipt.EventID = event.ID
			}
			supersedeInactiveTask(s, previousRunID, now)
			touchTicketIssue(s, t.ID, now)
		}
		e.invalidateApplications(s)
		if record.SnapshotID == "" {
			// No snapshot holds this ticket; keep it with the event.
			ticket := t
			record.Ticket = &ticket
		}
		s.Events.Put(event.ID, record)
		s.Receipts.Put(event.ID, receipt)
		return nil
	})
	return receipt, err
}

func (e *Engine) collectCase(s *State, t Ticket, now time.Time) Case {
	c := Case{
		ID:          "case_" + hash(t.ID)[:24],
		TicketID:    t.ID,
		ProjectID:   t.ProjectID,
		SourceKey:   t.SourceKey,
		Subject:     t.Subject,
		Summary:     t.Description,
		Components:  unique(t.Components),
		Versions:    unique(t.Versions),
		Environment: t.Environment,
		Resolution:  t.Resolution,
		State:       CaseDraft,
		CreatedAt:   now,
		UpdatedAt:   now,
		Version:     1,
	}
	c.IssueID = s.Runs.Get(s.Latest.Get(t.ID)).IssueID
	if override := s.IssueOverrides.Get(t.ID); override != "" {
		c.IssueID = override
	}
	c.SourceHash = hash(struct {
		Input      string
		Resolution Resolution
	}{diagnosticHash(t), t.Resolution})
	if lower(t.Resolution.Reason) == "duplicate" {
		c.State = CaseDuplicate
	} else if contains([]string{"rejected", "cancelled", "won't fix", "wontfix", "not a problem", "invalid"}, t.Resolution.Reason) {
		c.State = CaseArchived
	}
	if old, ok := s.Cases.Lookup(c.ID); ok {
		if c.IssueID == "" {
			c.IssueID = old.IssueID
		}
		if old.SourceHash == c.SourceHash && old.State != CaseInvalidated {
			return old
		}
		c.CreatedAt = old.CreatedAt
		c.Version = old.Version + 1
	}
	if sol, ok := canonicalSolution(*s, c.IssueID, caseScope(c)); ok && sol.State == SolutionTrusted && sameResolution(sol.Resolution, c.Resolution) {
		c.SolutionID = sol.ID
		c.State = CaseTrusted
		c.ReviewedBy = sol.Reviewer
		c.ReviewNote = sol.Note
		c.TrustedAt = sol.AvailableAt
	}
	s.Cases.Put(c.ID, c)
	s.CaseHistory = append(s.CaseHistory, c)
	return c
}

type Review struct {
	Action     string      `json:"action"`
	Reviewer   string      `json:"reviewer"`
	Note       string      `json:"note"`
	Resolution *Resolution `json:"resolution,omitempty"`
}

// ReviewCase publishes a reviewed case revision and reconciles linked solution trust.
// It does not automatically route unrelated open tickets or close Redmine issues.
func (e *Engine) ReviewCase(id string, review Review) (Case, error) {
	var result Case
	err := e.update(func(s *State) error {
		c, ok := s.Cases.Lookup(id)
		original := c
		if !ok {
			return ErrNotFound
		}
		if c.IssueID != "" {
			if _, exists := s.Issues.Lookup(c.IssueID); !exists {
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
			t, ok := s.Tickets.Lookup(c.TicketID)
			if !ok || !t.Closed || t.Private || c.State == CaseDuplicate || c.State == CaseArchived || c.State == CaseInvalidated {
				return fmt.Errorf("%w: source ticket is not eligible", ErrInvalid)
			}
			if c.Resolution.Cause == "" || c.Resolution.Solution == "" || c.Resolution.Validation == "" || len(c.Components) == 0 || len(c.Versions) == 0 {
				return fmt.Errorf("%w: confirmed cause, solution, verification and scope required", ErrInvalid)
			}
		}
		state := map[string]SolutionState{"approve": SolutionTrusted, "reject": SolutionRejected, "withdraw": SolutionWithdrawn}[review.Action]
		if sol, ok := canonicalSolution(*s, c.IssueID, caseScope(c)); ok && c.IssueID != "" {
			c.SolutionID = sol.ID
			s.Cases.Put(id, c)
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
				e.publishSolution(s, s.Investigations.Get(sol.InvestigationID), sol, next)
			}
			result = s.Cases.Get(id)
			if hash(result) != hash(original) && result.Version == original.Version {
				result.Version++
				result.UpdatedAt = e.Now()
				s.Cases.Put(id, result)
				s.CaseHistory = append(s.CaseHistory, result)
			}
			return nil
		}
		// Without a canonical solution the case takes the review state itself.
		if c.State == CaseState(state) && sameResolution(s.Cases.Get(id).Resolution, c.Resolution) {
			result = c
			return nil
		}
		c.State = CaseState(state)
		c.ReviewedBy = review.Reviewer
		c.ReviewNote = review.Note
		c.Version++
		c.UpdatedAt = e.Now()
		if state == SolutionTrusted {
			c.TrustedAt = e.Now()
		} else {
			c.TrustedAt = time.Time{}
		}
		s.Cases.Put(id, c)
		s.CaseHistory = append(s.CaseHistory, c)
		e.invalidateApplications(s)
		result = c
		return nil
	})
	return result, err
}

// AnalyzeTicket explicitly retries the current forwarded snapshot for a named actor.
// Unchanged input may reuse a run; force requests a new diagnosis.
func (e *Engine) AnalyzeTicket(id string, force bool, actor string) (Receipt, error) {
	ticket, ok := e.Store.Snapshot().Tickets.Lookup(id)
	if !ok || ticket.ForwardedBy == "" {
		return Receipt{}, ErrNotFound
	}
	if ticket.Closed {
		return Receipt{}, fmt.Errorf("%w: forward the latest closed snapshot to collect a case", ErrInvalid)
	}
	return e.Receive(Event{
		ID:          randomID("manual_"),
		Origin:      "manual_forward",
		ForwardedBy: actor,
		ForwardedAt: e.Now(),
		OccurredAt:  e.Now(),
		Force:       force,
		Ticket:      ticket,
	})
}
