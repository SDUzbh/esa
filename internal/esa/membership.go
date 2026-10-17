package esa

import (
	"fmt"
	"strings"
)

func activeMembership(s State, ticketID string) (IssueTicketLink, bool) {
	var found IssueTicketLink
	for _, id := range s.membershipsOfTicket(ticketID) {
		link := s.Memberships.Get(id)
		if link.State == "tentative" && (found.ID == "" || link.AvailableAt.After(found.AvailableAt) || link.AvailableAt.Equal(found.AvailableAt) && link.ID < found.ID) {
			found = link
		}
	}
	return found, found.ID != ""
}

// Membership review never forwards a snapshot, queues work or rewrites a report.
func (e *Engine) ReviewMembership(ticketID string, review MembershipReview) (Receipt, error) {
	var receipt Receipt
	err := e.update(func(s *State) error {
		t, ok := s.Tickets.Lookup(ticketID)
		if !ok || t.Private || t.Closed {
			return ErrNotFound
		}
		old, ok := activeMembership(*s, ticketID)
		if !ok || old.IssueID == "" {
			return fmt.Errorf("%w: complete analysis before reviewing membership", ErrConflict)
		}
		if review.ExpectedSnapshotID != s.LatestSnapshot.Get(ticketID) || review.ExpectedMembershipID != old.ID {
			return fmt.Errorf("%w: snapshot or membership changed", ErrConflict)
		}
		if strings.TrimSpace(review.Reviewer) == "" || strings.TrimSpace(review.Reason) == "" {
			return fmt.Errorf("%w: membership reviewer and reason required", ErrInvalid)
		}
		issue, exists := s.Issues.Lookup(review.IssueID)
		if review.IssueID != "" && (!exists || issue.ProjectID != t.ProjectID || issue.CreatedAt.After(e.Now())) {
			return fmt.Errorf("%w: issue outside project registry", ErrInvalid)
		}
		if exists && old.IssueID == issue.ID && old.Reviewer != "" {
			touchIssue(s, issue.ID, e.Now())
			receipt = Receipt{TicketID: ticketID, RunID: s.Latest.Get(ticketID), Action: "membership_unchanged"}
			return nil
		}
		if !exists {
			issue = EngineeringIssue{ID: randomID("issue_"), ProjectID: t.ProjectID, Title: t.Subject, TitleSourceTicketID: t.ID, CreatedAt: e.Now()}
			s.Issues.Put(issue.ID, issue)
		}
		s.IssueOverrides.Put(ticketID, issue.ID)
		touchIssue(s, issue.ID, e.Now())
		if old.IssueID == issue.ID {
			old.Version++
			old.Reviewer = review.Reviewer
			old.Basis = review.Reason
			old.AvailableAt = e.Now()
			s.Memberships.Put(old.ID, old)
			s.MembershipHistory = append(s.MembershipHistory, old)
		} else {
			old.State = "detached"
			old.Version++
			old.Reviewer = review.Reviewer
			old.Basis = review.Reason
			old.AvailableAt = e.Now()
			s.Memberships.Put(old.ID, old)
			s.MembershipHistory = append(s.MembershipHistory, old)
			link := IssueTicketLink{ID: randomID("link_"), IssueID: issue.ID, TicketID: ticketID, SnapshotID: old.SnapshotID, InvestigationID: old.InvestigationID, Version: 1, Reviewer: review.Reviewer, State: "tentative", Basis: review.Reason, AvailableAt: e.Now()}
			s.Memberships.Put(link.ID, link)
			s.MembershipHistory = append(s.MembershipHistory, link)
		}
		receipt = Receipt{TicketID: ticketID, RunID: s.Latest.Get(ticketID), Action: "membership_updated"}
		return nil
	})
	return receipt, err
}
