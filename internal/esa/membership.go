package esa

import (
	"fmt"
	"strings"
)

// Reviewed assignment is also the split/merge primitive. Sharing an issue ID
// never changes a frozen task or extends the scope of an existing solution.
func (e *Engine) ReviewMembership(ticketID string, review MembershipReview) (Receipt, error) {
	var receipt Receipt
	err := e.Store.Update(func(s *State) error {
		prepareInvestigations(s)
		t, ok := s.Tickets[ticketID]
		if !ok || t.Private || t.Closed {
			return ErrNotFound
		}
		if s.LatestSnapshot[ticketID] != review.ExpectedSnapshotID {
			return fmt.Errorf("%w: snapshot changed", ErrConflict)
		}
		if strings.TrimSpace(review.Reviewer) == "" || strings.TrimSpace(review.Reason) == "" {
			return fmt.Errorf("%w: membership reviewer and reason required", ErrInvalid)
		}
		issue, ok := s.Issues[review.IssueID]
		if review.IssueID != "" && (!ok || issue.ProjectID != t.ProjectID || issue.CreatedAt.After(e.Now())) {
			return fmt.Errorf("%w: engineering issue outside eligible project registry", ErrInvalid)
		}
		if !ok {
			issue = EngineeringIssue{ID: randomID("issue_"), ProjectID: t.ProjectID, Title: t.Subject, CreatedAt: e.Now()}
			s.Issues[issue.ID] = issue
		}
		for id, link := range s.Memberships {
			if link.TicketID == ticketID && link.State == "tentative" {
				link.State = "detached"
				link.Version++
				link.Reviewer = review.Reviewer
				link.Basis = review.Reason
				link.AvailableAt = e.Now()
				s.Memberships[id] = link
				s.MembershipHistory = append(s.MembershipHistory, link)
			}
		}
		s.IssueOverrides[ticketID] = issue.ID
		// An engineer's reassignment is a later authorized observation, not a
		// retroactive mutation of the initial snapshot or diagnosis cutoff.
		t.ForwardedAt, t.ForwardedBy = e.Now(), review.Reviewer
		s.Tickets[ticketID] = t
		event := Event{ID: randomID("membership_"), Origin: "reviewed_assignment", ForwardedBy: review.Reviewer, ForwardedAt: e.Now(), OccurredAt: e.Now(), ReceivedAt: e.Now(), Ticket: t}
		captureSnapshot(s, event)
		s.Events[event.ID] = event
		previousRunID := s.Latest[ticketID]
		delete(s.Latest, ticketID)
		receipt = e.enqueue(s, t, e.Now(), false)
		supersedeInactiveTask(s, previousRunID, e.Now())
		if receipt.Action == "created" {
			run := s.Runs[receipt.RunID]
			task := s.Investigations[run.InvestigationID]
			if task.ID != "" {
				task.Contract.ReviewContext = review.Reason
				s.Investigations[task.ID] = task
			}
		}
		receipt.EventID = event.ID
		s.Receipts[event.ID] = receipt
		for id, link := range s.Memberships {
			if link.TicketID == ticketID && link.State == "tentative" {
				link.Version++
				link.Reviewer = review.Reviewer
				link.Basis = "Reviewed membership: " + review.Reason
				link.AvailableAt = e.Now()
				s.Memberships[id] = link
				s.MembershipHistory = append(s.MembershipHistory, link)
			}
		}
		return nil
	})
	return receipt, err
}
