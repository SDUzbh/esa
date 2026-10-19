package esa

import (
	"errors"
	"log/slog"
	"time"
)

// ProviderQuotaError reports that the engineering provider refused a stage
// because its model account reached a usage or rate limit. The stage produced
// nothing and can run again after RetryAt; it is not a diagnosis failure.
type ProviderQuotaError struct {
	RetryAt time.Time
	Message string
}

func (e *ProviderQuotaError) Error() string {
	return "engineering provider usage limit reached; retrying after " + e.RetryAt.UTC().Format(time.RFC3339) + ": " + e.Message
}

// providerPaused reports whether new provider work must wait for a usage
// limit to reset. The provider's reset time is wall-clock time, never the
// replay clock.
func providerPaused(s State) bool {
	return time.Now().Before(s.ProviderPausedUntil)
}

// queueWaitStart is when a parked task's queue wait began counting: time
// spent waiting for the provider's usage limit to reset does not count.
func queueWaitStart(s State, task Investigation) time.Time {
	if s.ProviderPausedUntil.After(task.WaitingSince) {
		return s.ProviderPausedUntil
	}
	return task.WaitingSince
}

// holdForQuota pauses all provider work until the limit resets and keeps the
// task's progress: refused routing returns it to the queue; refused
// engineering or assignment parks it with its frozen input and the report of
// its last completed stage, so it resumes there.
func (e *Engine) holdForQuota(s *State, current *Investigation, run *Run, input AnalyzeRequest, quota *ProviderQuotaError, elapsed time.Duration) {
	if quota.RetryAt.After(s.ProviderPausedUntil) {
		if !time.Now().Before(s.ProviderPausedUntil) {
			slog.Warn("Engineering provider reached its usage limit; provider work is paused", "until", quota.RetryAt.UTC(), "message", quota.Message)
		}
		s.ProviderPausedUntil = quota.RetryAt
	}
	run.Error = quota.Error()
	run.UpdatedAt = e.Now()
	current.Owner, current.LeaseUntil, current.UpdatedAt = "", time.Time{}, e.Now()
	if current.Phase == "investigating" || current.Phase == "assignment" {
		frozen := input
		run.PipelineInput = &frozen
		current.Phase, current.WaitingSince = "waiting_engineering", time.Now().UTC()
		current.ElapsedMS = elapsed.Milliseconds()
		return
	}
	current.State, current.Phase = InvestigationQueued, ""
}

func asQuota(err error) (*ProviderQuotaError, bool) {
	var q *ProviderQuotaError
	ok := errors.As(err, &q)
	return q, ok
}
