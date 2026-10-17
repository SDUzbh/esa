package esa

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func inputIssueRecords(t *testing.T, e *Engine, in AnalyzeRequest) []IssueRecord {
	t.Helper()
	directory, err := OpenIssueDirectory(e.IssueDirectory, in.IssueDirectory, in.IssueDirectoryHash)
	if err != nil {
		t.Fatal(err)
	}
	rows := []IssueRecord{}
	for offset := 0; ; {
		page, total, err := directory.Search("", "", offset, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page {
			record, err := directory.ReadRecord(entry.Path, entry.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, record)
		}
		offset += len(page)
		if offset >= total {
			break
		}
	}
	raw, _ := json.Marshal(in)
	if strings.Contains(string(raw), `"known_issues"`) || strings.Contains(string(raw), `"cases"`) {
		t.Fatal("Issue payload was inlined", string(raw))
	}
	return rows
}
func reuseFirstIssue(t *testing.T, e *Engine, in AnalyzeRequest) Report {
	t.Helper()
	directory, err := OpenIssueDirectory(e.IssueDirectory, in.IssueDirectory, in.IssueDirectoryHash)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := directory.Search("", "", 0, 1)
	if err != nil || len(page) != 1 {
		t.Fatal("no reusable record", err)
	}
	record, err := directory.ReadRecord(page[0].Path, page[0].SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return ReusedIssueReport(record, RoutingResult{Action: "reuse", RecordPath: page[0].Path, RecordHash: page[0].SHA256, Reason: "Same defect and applicable environment", Usage: Usage{Calls: 1, InputTokens: 5, OutputTokens: 3, TokensReported: true}})
}

func TestIssueDirectoryPaginationIntegrityAndContainment(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []IssueRecord{}
	for i := 0; i < 25; i++ {
		records = append(records, IssueRecord{ID: fmt.Sprintf("issue_%d", i), IssueID: fmt.Sprintf("issue_%d", i), ProjectID: "1", ScopeHash: "scope", Kind: "agent_analyzed", Title: fmt.Sprintf("Failure %d", i), Summary: "UniqueException occurs in partition handoff", Recommendations: []string{"Guard assignment"}, AvailableAt: at})
	}
	relative, digest, err := ExportIssueDirectory(root, "1", at, records)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := OpenIssueDirectory(root, relative, digest)
	if err != nil {
		t.Fatal(err)
	}
	first, total, err := directory.Search("UniqueException", "", 0, 20)
	if err != nil || total != 25 || len(first) != 20 {
		t.Fatal(total, err)
	}
	second, total, err := directory.Search("UniqueException", "", 20, 20)
	if err != nil || len(second) != 5 || total != 25 {
		t.Fatal(total, err)
	}
	seen := map[string]bool{}
	for _, row := range append(first, second...) {
		if seen[row.Path] || row.SearchText != "" {
			t.Fatal("duplicate or full text leaked in search", row)
		}
		seen[row.Path] = true
	}
	for _, bad := range []string{"../manifest.json", "/etc/passwd", "agent_analyzed/../../../escape/issue.json"} {
		if _, err = directory.ReadRecord(bad, ""); err == nil {
			t.Fatal("unregistered path accepted", bad)
		}
	}
	if _, err = OpenIssueDirectory(root, "../outside", digest); err == nil {
		t.Fatal("directory traversal accepted")
	}
	external := t.TempDir()
	if err = os.Symlink(external, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenIssueDirectory(root, "escape", digest); err == nil {
		t.Fatal("symlink escaped registered root")
	}
	entry := first[0]
	if err = os.Chmod(filepath.Join(root, relative, entry.Path), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, relative, entry.Path), []byte(`{"solution":"tampered"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = directory.ReadRecord(entry.Path, entry.SHA256); err == nil {
		t.Fatal("changed solution accepted")
	}
}

func TestIssueDirectoryPreservesScopesAndPromotesEngineerSolution(t *testing.T) {
	e := testEngine(t)
	a := receive(t, e, "a", fixture(e, "a"))
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
		t.Fatal(err)
	}
	firstRun := e.Store.Snapshot().Runs.Get(a.RunID)
	second := fixture(e, "b")
	second.Versions = []string{"4.0.0"}
	b := receive(t, e, "b", second)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) {
		r := diagnosticReport()
		r.Issue.ExistingID = firstRun.IssueID
		return r, nil
	})); err != nil {
		t.Fatal(err)
	}
	state := e.Store.Snapshot()
	task := state.Investigations.Get(state.Runs.Get(b.RunID).InvestigationID)
	if _, err := e.ReviewSolution(task.ID, SolutionReview{Action: "approve", Reviewer: "engineer", ExpectedSolutionID: task.SolutionID, Resolution: Resolution{Cause: "Reviewed scope-specific cause", Solution: "Apply reviewed guard", Validation: "Version-specific regression"}}); err != nil {
		t.Fatal(err)
	}
	query := fixture(e, "query")
	query.Description += " new customer"
	receive(t, e, "query", query)
	if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
		rows := inputIssueRecords(t, e, in)
		if len(rows) != 2 {
			t.Fatal("scope records were collapsed", rows)
		}
		kinds := map[string]string{}
		for _, row := range rows {
			kinds[row.Versions[0]] = row.Kind
		}
		if kinds["3.7.0"] != "agent_analyzed" || kinds["4.0.0"] != "engineer_resolved" {
			t.Fatal(kinds)
		}
		return Report{Summary: "Fixture clarification", Limitations: []string{"No diagnosis"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryReuseRejectsWithdrawalDuringRouting(t *testing.T) {
	for _, kind := range []string{"agent", "engineer"} {
		t.Run(kind, func(t *testing.T) {
			e := testEngine(t)
			source := fixture(e, "source")
			if kind == "engineer" {
				seedRetrievalCase(t, e, source, true)
			} else {
				receive(t, e, "source", source)
				if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(context.Context, AnalyzeRequest) (Report, error) { return diagnosticReport(), nil })); err != nil {
					t.Fatal(err)
				}
			}
			query := fixture(e, "query")
			query.Description += " new report"
			receipt := receive(t, e, "query", query)
			if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) {
				report := reuseFirstIssue(t, e, in)
				if kind == "engineer" {
					id := inputIssueRecords(t, e, in)[0].CaseID
					if _, err := e.ReviewCase(id, Review{Action: "reject", Reviewer: "engineer"}); err != nil {
						t.Fatal(err)
					}
				} else {
					source.Private = true
					receive(t, e, "private", source)
				}
				return report, nil
			})); err != nil {
				t.Fatal(err)
			}
			state := e.Store.Snapshot()
			if state.Runs.Get(receipt.RunID).State != "failed" || state.Runs.Get(receipt.RunID).IssueID != "" {
				t.Fatal("withdrawn solution was published", state.Runs.Get(receipt.RunID))
			}
		})
	}
}

func TestDirectoryReuseCollectsStandaloneEngineerCaseOnce(t *testing.T) {
	e := testEngine(t)
	seedRetrievalCase(t, e, fixture(e, "source"), true)
	for _, id := range []string{"query-a", "query-b"} {
		query := fixture(e, id)
		query.Description += " " + id
		receipt := receive(t, e, id, query)
		if _, err := e.ProcessOne(context.Background(), analyzerFunc(func(_ context.Context, in AnalyzeRequest) (Report, error) { return reuseFirstIssue(t, e, in), nil })); err != nil {
			t.Fatal(err)
		}
		state := e.Store.Snapshot()
		if state.Runs.Get(receipt.RunID).Route != "issue_reuse" || state.Issues.Len() != 1 || state.Runs.Get(receipt.RunID).Report.Usage.Calls != 1 {
			t.Fatal("case reuse duplicated Issue or invoked extra analysis", state.Runs.Get(receipt.RunID))
		}
	}
}

func TestIssueDirectoryExcludesFutureAndPrivateSources(t *testing.T) {
	e := testEngine(t)
	base := e.Now()
	visible := seedRetrievalCase(t, e, fixture(e, "visible"), true)
	later := base.Add(time.Hour)
	e.Now = func() time.Time { return later }
	seedRetrievalCase(t, e, fixture(e, "future"), true)
	private := fixture(e, "private")
	seedRetrievalCase(t, e, private, true)
	private.Private = true
	receive(t, e, "hide", private)
	other := fixture(e, "other-project")
	other.ProjectID = "2"
	seedRetrievalCase(t, e, other, true)
	state := e.Store.Snapshot()
	query := fixture(e, "query")
	query.ForwardedAt = base
	task := Investigation{ProjectID: "1", Contract: TaskContract{ObservationCutoff: base}}
	records := issueRecords(state, task, query)
	if len(records) != 1 || records[0].CaseID != visible.ID {
		t.Fatal("future/private/other-project content entered directory", records)
	}
}
