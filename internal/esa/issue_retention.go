package esa

import (
	"sort"
	"time"
)

// IssueLimit caps live Engineering Issues across all projects, not tickets or audit records.
const IssueLimit = 100

// Every business mutation enforces retention in the same CAS transaction.
// Immutable snapshots, reports and solution revisions are never deleted.
func (e *Engine) update(fn func(*State) error) error {
	return e.Store.Update(func(s *State) error {
		if err := fn(s); err != nil {
			return err
		}
		if maintainIssues(s, e.Now()) {
			e.invalidateApplications(s)
		}
		return nil
	})
}

func touchIssue(s *State, id string, at time.Time) {
	issue, ok := s.Issues[id]
	if !ok || issue.CreatedAt.After(at) {
		return
	}
	s.IssueAccessSequence++
	issue.AccessOrder, issue.LastAccessedAt = s.IssueAccessSequence, at
	s.Issues[id] = issue
}

func touchTicketIssue(s *State, id string, at time.Time) {
	if link, ok := activeMembership(*s, id); ok && link.IssueID != "" {
		touchIssue(s, link.IssueID, at)
		return
	}
	if t := s.Tickets[id]; t.Closed {
		touchIssue(s, s.Cases["case_"+hash(id)[:24]].IssueID, at)
		return
	}
	touchIssue(s, s.Runs[s.Latest[id]].IssueID, at)
}

// maintainIssues removes empty Issues, then evicts by durable least-recent access.
// It detaches current associations while preserving immutable snapshots and reports.
func maintainIssues(s *State, at time.Time) bool {
	if len(s.Issues) == 0 {
		return false
	}
	live := map[string]bool{}
	assigned := map[string]string{}
	for _, link := range s.Memberships {
		t, ok := s.Tickets[link.TicketID]
		if !ok || t.Private || link.State != "tentative" || link.IssueID == "" {
			continue
		}
		live[link.IssueID] = true
		assigned[t.ID] = link.IssueID
	}
	for _, c := range s.Cases {
		t, ok := s.Tickets[c.TicketID]
		if ok && !t.Private && t.Closed && c.State != "invalidated" && c.State != "duplicate" && c.State != "archived" {
			live[c.IssueID] = true
		}
	}
	// A usable analysis still has a source ticket while an explicit replacement
	// task is unassigned. Do not erase that knowledge before routing can reuse it.
	for _, sol := range s.Solutions {
		if live[sol.IssueID] || s.Investigations[sol.InvestigationID].SolutionID != sol.ID || sol.State != "proposed" && sol.State != "trusted" {
			continue
		}
		t, ok := s.Tickets[sol.SourceTicketID]
		run := s.Runs[sol.RunID]
		if !ok || t.Private || t.Closed || run.TicketID != t.ID || investigationSignature(run.Input) != investigationSignature(t) {
			continue
		}
		primary := assigned[t.ID]
		if override := s.IssueOverrides[t.ID]; override != "" {
			primary = override
		}
		if primary == "" || primary == sol.IssueID {
			live[sol.IssueID] = true
		}
	}
	removed := false
	for id, issue := range s.Issues {
		if issue.AccessOrder > s.IssueAccessSequence {
			s.IssueAccessSequence = issue.AccessOrder
		}
		if !live[id] {
			removeIssue(s, id, "empty", at)
			removed = true
		}
	}
	if len(s.Issues) <= IssueLimit {
		return removed
	}
	issues := make([]EngineeringIssue, 0, len(s.Issues))
	for _, issue := range s.Issues {
		issues = append(issues, issue)
	}
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
	for _, issue := range issues[:len(issues)-IssueLimit] {
		removeIssue(s, issue.ID, "least recently used", at)
	}
	return true
}

func removeIssue(s *State, id, reason string, at time.Time) {
	delete(s.Issues, id)
	for key, link := range s.Memberships {
		if link.IssueID != id || link.State != "tentative" {
			continue
		}
		link.State, link.Basis, link.AvailableAt = "detached", "Issue removed: "+reason, at
		link.Version++
		s.Memberships[key] = link
		s.MembershipHistory = append(s.MembershipHistory, link)
	}
	for ticket, issue := range s.IssueOverrides {
		if issue == id {
			delete(s.IssueOverrides, ticket)
		}
	}
}
