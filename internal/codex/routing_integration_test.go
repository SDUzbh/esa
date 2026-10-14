package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"esa/internal/esa"
)

type applicationAnalyzer func(context.Context, esa.AnalyzeRequest) (esa.Report, error)

func (f applicationAnalyzer) Analyze(ctx context.Context, in esa.AnalyzeRequest) (esa.Report, error) {
	return f(ctx, in)
}

func TestNativeRoutingReusesAuthoritativeIssueOverHTTP(t *testing.T) {
	for _, kind := range []string{"agent", "engineer"} {
		t.Run(kind, func(t *testing.T) {
			provider, in, sourceCalls := routingFixture(t, "reuse_"+kind)
			raw, err := os.ReadFile("../../config/rules.json")
			if err != nil {
				t.Fatal(err)
			}
			var config esa.Config
			if err = json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			store, _ := esa.OpenStore("")
			engine, err := esa.NewEngine(store, config)
			if err != nil {
				t.Fatal(err)
			}
			engine.IssueDirectory = provider.IssuesRoot
			engine.Now = func() time.Time { return in.Contract.ObservationCutoff }
			source := in.Ticket
			source.ID = "source"
			source.SourceKey = "BUG-0"
			source.Subject = "Source assignment failure"
			source.Description = "NullPointerException during empty assignment"
			source.Components = []string{"consumer"}
			source.Environment = map[string]string{"jdk": "8"}
			source.CreatedAt = engine.Now()
			source.UpdatedAt = engine.Now()
			receipt, err := engine.Receive(esa.Event{ID: "source", Origin: "fixture", ForwardedBy: "human", ForwardedAt: engine.Now(), Ticket: source})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = engine.ProcessOne(context.Background(), applicationAnalyzer(func(context.Context, esa.AnalyzeRequest) (esa.Report, error) {
				return esa.Report{Issue: &esa.IssueAssignment{Title: "Known assignment defect", Reason: "Inspected source"}, Summary: "Stored authoritative diagnosis", Evidence: []esa.Evidence{{Source: "git", Reference: "old:Example.java:1", Quote: "bug();"}}, Recommendations: []string{"Stored authoritative guard"}, VerificationSteps: []string{"Stored regression"}, Usage: esa.Usage{Calls: 1, TokensReported: true}}, nil
			})); err != nil {
				t.Fatal(err)
			}
			first := store.Snapshot().Runs[receipt.RunID]
			if kind == "engineer" {
				task := store.Snapshot().Investigations[first.InvestigationID]
				if _, err = engine.ReviewSolution(task.ID, esa.SolutionReview{Action: "approve", Reviewer: "engineer", ExpectedSolutionID: task.SolutionID, Resolution: esa.Resolution{Cause: "Reviewed authoritative cause", Solution: "Engineer authoritative guard", Validation: "Reviewed regression"}}); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request esa.AnalyzeRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "Invalid fixture request", 400)
					return
				}
				report, err := provider.Analyze(r.Context(), request)
				if err != nil {
					http.Error(w, err.Error(), 422)
					return
				}
				_ = json.NewEncoder(w).Encode(report)
			}))
			defer server.Close()
			query := source
			query.ID = "consumer"
			query.SourceKey = "BUG-1"
			query.Description += " A second customer reproduction"
			next, err := engine.Receive(esa.Event{ID: "consumer", Origin: "fixture", ForwardedBy: "human", ForwardedAt: engine.Now(), Ticket: query})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = engine.ProcessOne(context.Background(), esa.HTTPPipelineAnalyzer{HTTPAnalyzer: esa.HTTPAnalyzer{URL: server.URL, Client: server.Client()}}); err != nil {
				t.Fatal(err)
			}
			state := store.Snapshot()
			run := state.Runs[next.RunID]
			if run.Route != "issue_reuse" || run.IssueID != first.IssueID || run.SourceTicketID != source.ID || run.Report.Routing == nil || run.Report.Usage.Calls != 1 || *sourceCalls != 0 || len(state.Issues) != 1 {
				t.Fatal("native routing did not reuse authoritative solution", run, *sourceCalls)
			}
			want := "Stored authoritative guard"
			if kind == "engineer" {
				want = "Engineer authoritative guard"
			}
			if len(run.Report.Recommendations) != 1 || run.Report.Recommendations[0] != want {
				t.Fatal("stored solution changed", run.Report)
			}
		})
	}
}

func TestNativeEngineeringRequestsRoutingAssignmentOverHTTP(t *testing.T) {
	for _, decision := range []string{"analyze", "assign_existing", "assignment_info", "assignment_unread"} {
		t.Run(decision, func(t *testing.T) {
			provider, in, sourceCalls := routingFixture(t, decision)
			raw, err := os.ReadFile("../../config/rules.json")
			if err != nil {
				t.Fatal(err)
			}
			var config esa.Config
			if err = json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			store, _ := esa.OpenStore("")
			engine, err := esa.NewEngine(store, config)
			if err != nil {
				t.Fatal(err)
			}
			engine.IssueDirectory = provider.IssuesRoot
			engine.Now = func() time.Time { return in.Contract.ObservationCutoff }
			source := in.Ticket
			source.ID, source.SourceKey = "source", "BUG-0"
			source.Description = "An earlier reproduction"
			source.CreatedAt, source.UpdatedAt = engine.Now(), engine.Now()
			seed, err := engine.Receive(esa.Event{ID: "source", Origin: "fixture", ForwardedBy: "human", ForwardedAt: engine.Now(), Ticket: source})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = engine.ProcessOne(context.Background(), applicationAnalyzer(func(context.Context, esa.AnalyzeRequest) (esa.Report, error) {
				return esa.Report{Issue: &esa.IssueAssignment{Title: "Earlier known defect", Reason: "Fixture controller decision"}, Summary: "Earlier defect", Evidence: []esa.Evidence{{Source: "git", Reference: "old:Example.java:1", Quote: "bug();"}}, Recommendations: []string{"Earlier recommendation"}, Usage: esa.Usage{Calls: 1, TokensReported: true}}, nil
			})); err != nil {
				t.Fatal(err)
			}
			original := store.Snapshot().Runs[seed.RunID]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input esa.AnalyzeRequest
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				report, err := provider.Analyze(r.Context(), input)
				if err != nil {
					w.WriteHeader(422)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "report": report})
					return
				}
				_ = json.NewEncoder(w).Encode(report)
			}))
			defer server.Close()
			query := source
			query.ID, query.SourceKey, query.Description = "query", "BUG-1", "New reproduction requiring code analysis"
			receipt, err := engine.Receive(esa.Event{ID: "query", Origin: "fixture", ForwardedBy: "human", ForwardedAt: engine.Now(), Ticket: query})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = engine.ProcessOne(context.Background(), esa.HTTPPipelineAnalyzer{HTTPAnalyzer: esa.HTTPAnalyzer{URL: server.URL, Client: server.Client()}}); err != nil {
				t.Fatal(err)
			}
			state := store.Snapshot()
			run := state.Runs[receipt.RunID]
			if run.Report.Usage.Calls != 3 || run.Report.Usage.InputTokens != 36 || *sourceCalls != 1 || len(run.Report.Findings) != 1 || run.Report.IssueRequest == nil {
				t.Fatal("request or stage accounting lost at public boundary", run)
			}
			switch decision {
			case "analyze":
				if run.State != "awaiting_review" || run.IssueID == original.IssueID || len(state.Issues) != 2 || state.Issues[run.IssueID].Title != "Routing approved canonical title" {
					t.Fatal("Routing Agent creation was not applied", run)
				}
			case "assign_existing":
				if run.State != "awaiting_review" || run.IssueID != original.IssueID || len(state.Issues) != 1 || state.Investigations[run.InvestigationID].Application != nil || len(state.Solutions) != 2 {
					t.Fatal("supported engineering result was not grouped into existing Issue", run)
				}
				request := httptest.NewRequest("GET", "/v1/tickets/query", nil)
				request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 24))
				request.Header.Set("X-ESA-Project-IDs", "1")
				request.Header.Set("X-ESA-Ticket-IDs", "query")
				response := httptest.NewRecorder()
				(&esa.API{Engine: engine, Token: strings.Repeat("x", 24)}).Handler().ServeHTTP(response, request)
				if response.Code != 200 || strings.Contains(response.Body.String(), `"quote":`) || strings.Contains(response.Body.String(), "The inspected diagnosis describes") {
					t.Fatal("grouping evidence crossed source visibility", response.Body)
				}
			case "assignment_info":
				if run.State != "awaiting_information" || run.IssueID != "" || len(state.Issues) != 1 || len(state.Solutions) != 1 {
					t.Fatal("clarification created an Issue or lost diagnosis", run)
				}
			case "assignment_unread":
				if run.State != "failed" || run.IssueID != "" || len(state.Issues) != 1 || len(state.Solutions) != 1 {
					t.Fatal("failed grouping fell back to engineering assignment", run)
				}
			}
		})
	}
}
