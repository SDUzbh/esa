package esa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUsageLimitHoldsTheTicketAndResumesAtTheRefusedStage(t *testing.T) {
	for _, refused := range []string{"route", "engineer", "assign"} {
		t.Run(refused, func(t *testing.T) {
			e := queueEngine(t, 1)
			e.Config.Worker.MaxQueueWaitSeconds = 60
			var mu sync.Mutex
			calls := map[string]int{}
			provider := pipelineFunc(func(_ context.Context, in AnalyzeRequest, stage string) (Report, error) {
				mu.Lock()
				calls[stage]++
				first := calls[stage] == 1
				mu.Unlock()
				if stage == refused && first {
					return Report{}, &ProviderQuotaError{RetryAt: time.Now().Add(time.Hour), Message: "You've hit your usage limit"}
				}
				switch stage {
				case "route":
					return intakeReport("analyze"), nil
				case "engineer":
					return pipelineEngineeringReport(), nil
				}
				return pipelineDiagnosticReport(), nil
			})
			runID := e.Store.Snapshot().Runs.All()
			var id string
			for k := range runID {
				id = k
			}
			if worked, err := e.ProcessOne(context.Background(), provider); !worked || err != nil {
				t.Fatal(worked, err)
			}
			s := e.Store.Snapshot()
			task, run := taskOf(s, id), s.Runs.Get(id)
			if !providerPaused(s) || !strings.Contains(run.Error, "usage limit") {
				t.Fatalf("provider work was not paused: %v %q", s.ProviderPausedUntil, run.Error)
			}
			if refused == "route" {
				if task.State != InvestigationQueued || task.Owner != "" {
					t.Fatalf("a refused routing turn must return the ticket to the queue: %+v", task)
				}
			} else if !parked(task) || run.PipelineInput == nil {
				t.Fatalf("refused %s must park the task with its frozen input: %+v", refused, task)
			}
			// While paused: no provider call, and the queue-wait limit does not fail the task.
			if err := e.update(func(s *State) error {
				current := s.Investigations.Get(task.ID)
				current.WaitingSince = time.Now().Add(-time.Hour)
				s.Investigations.Put(task.ID, current)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := calls[refused]
			if worked, err := e.ProcessOne(context.Background(), provider); worked || err != nil || calls[refused] != before {
				t.Fatalf("provider work ran while paused: %v %v", worked, err)
			}
			if state := taskOf(e.Store.Snapshot(), id).State; state == InvestigationFailed {
				t.Fatal("a ticket waiting for the usage limit timed out")
			}
			// After the reset the ticket resumes at the refused stage and completes.
			if err := e.update(func(s *State) error { s.ProviderPausedUntil = time.Now().Add(-time.Second); return nil }); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3 && taskOf(e.Store.Snapshot(), id).State != InvestigationAgentProposed; i++ {
				if _, err := e.ProcessOne(context.Background(), provider); err != nil {
					t.Fatal(err)
				}
			}
			s = e.Store.Snapshot()
			if task, run = taskOf(s, id), s.Runs.Get(id); task.State != InvestigationAgentProposed || run.Error != "" || len(run.Report.Findings) == 0 {
				t.Fatalf("resumed ticket did not complete: %s %q", task.State, run.Error)
			}
			want := map[string]map[string]int{
				"route":    {"route": 2, "engineer": 1, "assign": 1},
				"engineer": {"route": 1, "engineer": 2, "assign": 1},
				// The provider returns its cached engineering report; only assignment repeats.
				"assign": {"route": 1, "engineer": 2, "assign": 2},
			}[refused]
			for stage, n := range want {
				if calls[stage] != n {
					t.Errorf("%s calls = %d, want %d", stage, calls[stage], n)
				}
			}
		})
	}
}

func TestProviderUsageLimitResponseBecomesAQuotaError(t *testing.T) {
	reset := time.Date(2026, 10, 19, 15, 19, 0, 0, time.UTC)
	for name, respond := range map[string]func(http.ResponseWriter){
		"retry_at in the body": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "You've hit your usage limit", "retry_at": reset})
		},
		"Retry-After only": func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { respond(w) }))
		_, err := HTTPAnalyzer{URL: server.URL, Client: server.Client()}.Analyze(context.Background(), AnalyzeRequest{RunID: "run_x", Budget: Budget{TimeoutSeconds: 5}})
		server.Close()
		var quota *ProviderQuotaError
		if !errors.As(err, &quota) {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "retry_at in the body" && !quota.RetryAt.Equal(reset) {
			t.Errorf("%s: %v", name, quota.RetryAt)
		}
		if name == "Retry-After only" && (quota.RetryAt.Before(time.Now().Add(100*time.Second)) || quota.RetryAt.After(time.Now().Add(130*time.Second))) {
			t.Errorf("%s: %v", name, quota.RetryAt)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(422) }))
	defer server.Close()
	if _, err := (HTTPAnalyzer{URL: server.URL, Client: server.Client()}).Analyze(context.Background(), AnalyzeRequest{Budget: Budget{TimeoutSeconds: 5}}); err == nil || errors.As(err, new(*ProviderQuotaError)) {
		t.Fatal("other provider failures must stay failures", err)
	}
}
