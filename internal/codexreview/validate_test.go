package codexreview

import (
	"strings"
	"testing"

	"esa/internal/esa"
)

func reviewFixture() (Review, ReviewRequest) {
	request := ReviewRequest{
		Prediction: esa.Report{Findings: []esa.Finding{{Problem: "absent assignment"}}},
		Candidate:  Candidate{Commit: "abc123", PRs: []string{"#42"}},
		Documents:  []Document{{Source: "git", Prefix: "diff:Example.java:", First: 10, Lines: []string{"if (assignment == null) return;", "assignment.size();"}}},
	}
	review := Review{
		Commit:      "abc123",
		Verdict:     "resolved",
		Explanation: "The candidate guards the absent assignment.",
		Findings:    []FindingReview{{Index: 0, Localization: "correct", Problem: "correct", Solution: "correct", Explanation: "Same line"}},
		Evidence:    []esa.Evidence{{Source: "git", Reference: "diff:Example.java:10", Quote: "assignment == null"}},
	}
	return review, request
}

func TestValidateReviewAcceptsAGroundedReview(t *testing.T) {
	review, request := reviewFixture()
	if err := ValidateReview(review, request); err != nil {
		t.Fatal(err)
	}
	review.Verdict = "uncertain"
	review.Findings[0] = FindingReview{Index: 0, Localization: "unknown", Problem: "unknown", Solution: "unknown", Explanation: "Not confirmed"}
	if err := ValidateReview(review, request); err != nil {
		t.Fatal("an unconfirmed verdict that leaves accuracy unknown was rejected:", err)
	}
}

func TestValidateReviewRejectsUngroundedReviews(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*Review, *ReviewRequest)
		want   string
	}{
		"another commit":          {func(r *Review, _ *ReviewRequest) { r.Commit = "def456" }, "registered candidate"},
		"no explanation":          {func(r *Review, _ *ReviewRequest) { r.Explanation = " " }, "explain its verdict"},
		"unknown verdict":         {func(r *Review, _ *ReviewRequest) { r.Verdict = "fixed" }, "invalid repair verdict"},
		"resolved without a PR":   {func(_ *Review, q *ReviewRequest) { q.Candidate.PRs = nil }, "requires a PR reference"},
		"finding not assessed":    {func(r *Review, _ *ReviewRequest) { r.Findings = nil }, "every original finding"},
		"finding index too large": {func(r *Review, _ *ReviewRequest) { r.Findings[0].Index = 1 }, "invalid finding assessment"},
		"finding twice": {func(r *Review, q *ReviewRequest) {
			q.Prediction.Findings = append(q.Prediction.Findings, esa.Finding{})
			r.Findings = append(r.Findings, r.Findings[0])
		}, "invalid finding assessment"},
		"finding without reason":  {func(r *Review, _ *ReviewRequest) { r.Findings[0].Explanation = "" }, "invalid finding assessment"},
		"unknown judgment":        {func(r *Review, _ *ReviewRequest) { r.Findings[0].Problem = "partly" }, "invalid accuracy judgment"},
		"unconfirmed but judged":  {func(r *Review, _ *ReviewRequest) { r.Verdict = "not_resolved" }, "leave initial accuracy unknown"},
		"no evidence":             {func(r *Review, _ *ReviewRequest) { r.Evidence = nil }, "cite supplied evidence"},
		"quote not on the line":   {func(r *Review, _ *ReviewRequest) { r.Evidence[0].Quote = "assignment.size()" }, "does not match"},
		"line outside document":   {func(r *Review, _ *ReviewRequest) { r.Evidence[0].Reference = "diff:Example.java:99" }, "does not match"},
		"reference with a suffix": {func(r *Review, _ *ReviewRequest) { r.Evidence[0].Reference = "diff:Example.java:10x" }, "does not match"},
		"another source":          {func(r *Review, _ *ReviewRequest) { r.Evidence[0].Source = "web" }, "does not match"},
		"empty quote":             {func(r *Review, _ *ReviewRequest) { r.Evidence[0].Quote = " " }, "does not match"},
	} {
		review, request := reviewFixture()
		tc.change(&review, &request)
		if err := ValidateReview(review, request); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}
