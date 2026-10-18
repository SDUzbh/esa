package esa

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"time"
)

// IssueLimit caps active Engineering Issues without reviewed knowledge across
// all projects. Issues holding a trusted solution or case are never evicted by
// count; once solved and unmatched for the retirement period they are retired
// to the directory archive instead.
const IssueLimit = 100

// Every business mutation enforces retention in the same CAS transaction.
// Immutable snapshots, reports and solution revisions are never deleted.
func (e *Engine) update(fn func(*State) error) error {
	return e.Store.Update(func(s *State) error {
		if err := fn(s); err != nil {
			return err
		}
		if maintainIssues(s, e.Now(), e.retirementPeriod()) {
			e.invalidateApplications(s)
		}
		return nil
	})
}

func touchIssue(s *State, id string, at time.Time) {
	issue, ok := s.Issues.Lookup(id)
	if !ok || issue.CreatedAt.After(at) {
		return
	}
	s.IssueAccessSequence++
	issue.AccessOrder, issue.LastAccessedAt = s.IssueAccessSequence, at
	s.Issues.Put(id, issue)
}

// ticketTouchTarget returns the Issue that an access to the ticket would touch,
// or "" when touchTicketIssue would change nothing. It only reads s.
func ticketTouchTarget(s State, id string, at time.Time) string {
	issueID := s.Runs.Get(s.Latest.Get(id)).IssueID
	if link, ok := activeMembership(s, id); ok && link.IssueID != "" {
		issueID = link.IssueID
	} else if s.Tickets.Get(id).Closed {
		issueID = s.Cases.Get("case_" + hash(id)[:24]).IssueID
	}
	if issue, ok := s.Issues.Lookup(issueID); !ok || issue.CreatedAt.After(at) {
		return ""
	}
	return issueID
}

func touchTicketIssue(s *State, id string, at time.Time) {
	if issueID := ticketTouchTarget(*s, id, at); issueID != "" {
		touchIssue(s, issueID, at)
	}
}

// issueRetentionReference, when set (by tests), is a scanning implementation
// of maintainIssues that the guard runs on a copy and compares with the result.
var issueRetentionReference func(*State, time.Time, time.Duration) bool

// retirementPeriod is how long a solved Issue may go unmatched before it is
// retired (Config.IssueRetirementDays).
func (e *Engine) retirementPeriod() time.Duration {
	return time.Duration(e.Config.IssueRetirementDays) * 24 * time.Hour
}

// maintainIssues removes empty Issues, retires solved Issues that went
// unmatched for retireAfter, then evicts unreviewed Issues by durable
// least-recent access. It detaches current associations while preserving
// immutable snapshots and reports. It walks Issues and asks the by-Issue
// indexes for the records that decide each one, never the whole archive.
func maintainIssues(s *State, at time.Time, retireAfter time.Duration) bool {
	if s.Issues.Len() == 0 {
		return false
	}
	if !guardPublishedState || issueRetentionReference == nil {
		return maintainIndexedIssues(s, at, retireAfter)
	}
	ref := s.detach()
	ref.MembershipHistory, ref.CaseHistory = slices.Clip(ref.MembershipHistory), slices.Clip(ref.CaseHistory)
	wantRemoved := issueRetentionReference(&ref, at, retireAfter)
	removed := maintainIndexedIssues(s, at, retireAfter)
	got, _ := json.Marshal(*s)
	want, _ := json.Marshal(ref)
	if removed != wantRemoved || !bytes.Equal(got, want) {
		panic("esa: indexed Issue retention diverged from the full scan")
	}
	return removed
}

func maintainIndexedIssues(s *State, at time.Time, retireAfter time.Duration) bool {
	// Decide every Issue's liveness before removing any: a removal detaches
	// memberships, which must not change another Issue's verdict.
	var dead []string
	for _, id := range s.Issues.Keys() {
		issue := s.Issues.Get(id)
		if issue.AccessOrder > s.IssueAccessSequence {
			s.IssueAccessSequence = issue.AccessOrder
		}
		if !issueLive(*s, id) {
			dead = append(dead, id)
		}
	}
	for _, id := range dead {
		removeIssue(s, id, "empty", at)
	}
	removed := len(dead) > 0
	// Retire solved Issues nobody matched or opened for the retirement period.
	// They stay in the directory's archive, and a later match revives them.
	var capped []EngineeringIssue
	for _, id := range s.Issues.Keys() {
		issue := s.Issues.Get(id)
		if !issue.RetiredAt.IsZero() {
			continue
		}
		reviewed := issueReviewed(*s, id)
		if reviewed && !issueHasOpenMember(*s, id) && !lastIssueActivity(issue).Add(retireAfter).After(at) {
			issue.RetiredAt = at
			s.Issues.Put(id, issue)
			continue
		}
		if !reviewed {
			capped = append(capped, issue)
		}
	}
	if len(capped) <= IssueLimit {
		return removed
	}
	for _, issue := range leastRecent(capped) {
		removeIssue(s, issue.ID, "least recently used", at)
	}
	return true
}

// issueReviewed reports whether the Issue holds engineer-reviewed knowledge:
// a trusted current solution or a trusted case.
func issueReviewed(s State, id string) bool {
	x := s.indexes
	if x.usableSolutionsByIssue.exists(s.Solutions, usableSolutionIssue, id, func(sol SolutionRevision) bool {
		return sol.State == "trusted" && s.Investigations.Get(sol.InvestigationID).SolutionID == sol.ID
	}) {
		return true
	}
	return x.retainedCasesByIssue.exists(s.Cases, retainedCaseIssue, id, func(c Case) bool { return c.State == "trusted" })
}

// issueHasOpenMember reports whether a ticket still open is a current member.
func issueHasOpenMember(s State, id string) bool {
	return s.indexes.tentativeByIssue.exists(s.Memberships, tentativeIssue, id, func(link IssueTicketLink) bool {
		t, ok := s.Tickets.Lookup(link.TicketID)
		return ok && !t.Closed
	})
}

// lastIssueActivity is the last time the Issue was created, matched or opened.
func lastIssueActivity(issue EngineeringIssue) time.Time {
	latest := issue.CreatedAt
	for _, at := range []time.Time{issue.LastAccessedAt, issue.RevivedAt} {
		if at.After(latest) {
			latest = at
		}
	}
	return latest
}

// issueLive reports whether a current membership, a closed ticket's case, or
// a still-usable analysis keeps the Issue.
func issueLive(s State, id string) bool {
	x := s.indexes
	visible := func(ticketID string) (Ticket, bool) {
		t, ok := s.Tickets.Lookup(ticketID)
		return t, ok && !t.Private
	}
	if x.tentativeByIssue.exists(s.Memberships, tentativeIssue, id, func(link IssueTicketLink) bool {
		_, ok := visible(link.TicketID)
		return ok
	}) {
		return true
	}
	if x.retainedCasesByIssue.exists(s.Cases, retainedCaseIssue, id, func(c Case) bool {
		t, ok := visible(c.TicketID)
		return ok && t.Closed
	}) {
		return true
	}
	// A usable analysis still has a source ticket while an explicit replacement
	// task is unassigned. Do not erase that knowledge before routing can reuse it.
	return x.usableSolutionsByIssue.exists(s.Solutions, usableSolutionIssue, id, func(sol SolutionRevision) bool {
		return keepsIssue(s, sol)
	})
}

// keepsIssue is the analysis condition of issueLive for a proposed or
// trusted solution.
func keepsIssue(s State, sol SolutionRevision) bool {
	if s.Investigations.Get(sol.InvestigationID).SolutionID != sol.ID {
		return false
	}
	t, ok := s.Tickets.Lookup(sol.SourceTicketID)
	run := s.Runs.Get(sol.RunID)
	if !ok || t.Private || t.Closed || run.TicketID != t.ID || investigationSignature(run.Input) != investigationSignature(t) {
		return false
	}
	var primary string
	if link, ok := activeMembership(s, t.ID); ok {
		primary = link.IssueID
	}
	if override := s.IssueOverrides.Get(t.ID); override != "" {
		primary = override
	}
	return primary == "" || primary == sol.IssueID
}

// leastRecent returns the issues beyond IssueLimit, least recently accessed
// first.
func leastRecent(issues []EngineeringIssue) []EngineeringIssue {
	issues = slices.Clone(issues)
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.AccessOrder != b.AccessOrder {
			return a.AccessOrder < b.AccessOrder
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return issues[:len(issues)-IssueLimit]
}

func removeIssue(s *State, id, reason string, at time.Time) {
	s.Issues.Delete(id)
	for _, key := range s.indexes.tentativeByIssue.lookup(s.Memberships, tentativeIssue, id) {
		detachForRemovedIssue(s, key, reason, at)
	}
	for _, ticket := range s.indexes.overridesByIssue.lookup(s.IssueOverrides, overrideIssue, id) {
		s.IssueOverrides.Delete(ticket)
	}
}

func detachForRemovedIssue(s *State, key, reason string, at time.Time) {
	link := s.Memberships.Get(key)
	link.State, link.Basis, link.AvailableAt = "detached", "Issue removed: "+reason, at
	link.Version++
	s.Memberships.Put(key, link)
	s.MembershipHistory = append(s.MembershipHistory, link)
}
