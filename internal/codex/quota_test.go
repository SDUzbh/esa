package codex

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"esa/internal/esa"
)

func TestUsageLimitMessagesAndResetTimes(t *testing.T) {
	for message, want := range map[string]bool{
		"You've hit your usage limit. Upgrade to Pro or try again at 11:19 PM.": true,
		"Rate limit reached for requests":                                       true,
		"stream disconnected before completion":                                 false,
		"Codex failed; inspect private run logs":                                false,
	} {
		if usageLimited(message) != want {
			t.Errorf("usageLimited(%q) != %v", message, want)
		}
	}
	now := time.Date(2026, 10, 19, 20, 5, 0, 0, time.Local)
	for message, want := range map[string]time.Time{
		"try again at 11:19 PM.":       time.Date(2026, 10, 19, 23, 19, 0, 0, time.Local),
		"try again at 8:00 AM":         time.Date(2026, 10, 20, 8, 0, 0, 0, time.Local),  // tomorrow
		"try again at 12:30 a.m.":      time.Date(2026, 10, 20, 0, 30, 0, 0, time.Local), // midnight
		"try again at 20:04":           time.Date(2026, 10, 20, 20, 4, 0, 0, time.Local), // just passed
		"Please try again in 3 hours.": now.Add(3 * time.Hour),
		"retry in 45 minutes":          now.Add(45 * time.Minute),
		"usage limit reached":          now.Add(time.Hour),
		"try again in 10 seconds":      now.Add(time.Minute),
		"try again in 30 days":         now.Add(7 * 24 * time.Hour),
	} {
		if got := quotaReset(message, now); !got.Equal(want) {
			t.Errorf("quotaReset(%q) = %v, want %v", message, got, want)
		}
	}
}

// staged sends one pipeline stage for the fixture's run.
func staged(p *Provider, in esa.AnalyzeRequest, stage string) (esa.Report, error) {
	in.Stage = stage
	return p.Analyze(context.Background(), in)
}

func TestARefusedStageRunsAgainAfterTheUsageLimit(t *testing.T) {
	for _, refused := range []string{"route", "engineer", "assign"} {
		t.Run(refused, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, "quota_"+refused)
			t.Setenv("QUOTA_MARKER", filepath.Join(t.TempDir(), "refused"))
			var err error
			for _, stage := range []string{"route", "engineer", "assign"} {
				if _, err = staged(p, in, stage); err != nil {
					break
				}
			}
			var quota *esa.ProviderQuotaError
			if !errors.As(err, &quota) || quota.RetryAt.Hour() != 23 || quota.RetryAt.Minute() != 19 || !strings.Contains(quota.Message, "usage limit") {
				t.Fatalf("refusal was not reported as a usage limit: %v", err)
			}
			// The refused attempt stays in the audit under a new name.
			aside, _ := filepath.Glob(filepath.Join(p.RunsDir, "*", "*.quota-*"))
			if refused == "route" {
				aside, _ = filepath.Glob(filepath.Join(p.RunsDir, in.RunID+".quota-*"))
			}
			if len(aside) == 0 {
				t.Fatal("refused attempt was not set aside")
			}
			// ESA retries from the refused stage; earlier stages return their cached reports.
			start := map[string]int{"route": 0, "engineer": 1, "assign": 1}[refused]
			var report esa.Report
			for _, stage := range []string{"route", "engineer", "assign"}[start:] {
				if report, err = staged(p, in, stage); err != nil {
					t.Fatalf("%s after the reset: %v", stage, err)
				}
			}
			if len(report.Findings) != 1 || report.AssignmentRouting == nil {
				t.Fatalf("the resumed run did not complete: %+v", report)
			}
			if want := map[string]int{"route": 1, "engineer": 2, "assign": 1}[refused]; *sourceCalls != want {
				t.Fatalf("engineering ran %d times, want %d", *sourceCalls, want)
			}
		})
	}
}

func TestUsageLimitIsAnsweredWith429(t *testing.T) {
	p, in, _ := routingFixture(t, "quota_route")
	t.Setenv("QUOTA_MARKER", filepath.Join(t.TempDir(), "refused"))
	server := httptest.NewServer(p.Handler("token"))
	defer server.Close()
	analyzer := esa.HTTPPipelineAnalyzer{HTTPAnalyzer: esa.HTTPAnalyzer{URL: server.URL + "/analyze", Token: "token", Client: server.Client()}}
	_, err := analyzer.AnalyzeStage(context.Background(), in, "route")
	var quota *esa.ProviderQuotaError
	if !errors.As(err, &quota) || quota.RetryAt.Local().Hour() != 23 || !strings.Contains(quota.Message, "usage limit") {
		t.Fatalf("ESA did not receive a usage-limit error: %v", err)
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/analyze", bytes.NewReader(mustJSON(withStage(in, "route"))))
	request.Header.Set("Authorization", "Bearer token")
	os.Remove(os.Getenv("QUOTA_MARKER"))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", response.StatusCode, response.Header.Get("Retry-After"))
	}
}

func withStage(in esa.AnalyzeRequest, stage string) esa.AnalyzeRequest {
	in.Stage = stage
	return in
}
