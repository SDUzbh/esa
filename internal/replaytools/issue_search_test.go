package replaytools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"esa/internal/esa"
)

func issueSearchFixture(t *testing.T) Server {
	t.Helper()
	root := t.TempDir()
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	record := func(id, summary string) esa.IssueRecord {
		return esa.IssueRecord{ID: id, IssueID: id, ProjectID: "1", ScopeHash: "scope", Kind: "agent_analyzed", Title: id,
			Summary: summary, Recommendations: []string{"Fix it"}, AvailableAt: at}
	}
	records := []esa.IssueRecord{
		record("issue_deadlock", "Heartbeat expiry deadlock with offset commit in GroupMetadataManager"),
		record("issue_npe", "NullPointerException in JsonConverter on a null logical value"),
		record("issue_both", "Deadlock after a NullPointerException in the coordinator"),
	}
	relative, digest, err := esa.ExportIssueDirectory(root, "1", at, records)
	if err != nil {
		t.Fatal(err)
	}
	return Server{Config: Config{IssuesOnly: true, IssuesRoot: root, IssueDirectory: relative, IssueDirectoryHash: digest,
		AuditPath: filepath.Join(t.TempDir(), "tools.jsonl")}}
}

func TestIssueSearchRunsSeveralQueriesInOneCall(t *testing.T) {
	s := issueSearchFixture(t)
	result, err := s.Call(context.Background(), "issue_search", json.RawMessage(`{"queries":["deadlock","NullPointerException","deadlock","no such words"]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	var got struct {
		Entries []struct {
			Title   string   `json:"title"`
			Path    string   `json:"path"`
			SHA256  string   `json:"sha256"`
			Matched []string `json:"matched_queries"`
		} `json:"entries"`
		Totals map[string]int `json:"totals"`
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 3 || got.Totals["deadlock"] != 2 || got.Totals["NullPointerException"] != 2 || got.Totals["no such words"] != 0 || len(got.Totals) != 3 {
		t.Fatal("hits were not merged once each with per-query totals", string(raw))
	}
	matched := map[string][]string{}
	for _, e := range got.Entries {
		matched[e.Title] = e.Matched
		if e.Path == "" || e.SHA256 == "" {
			t.Fatal("a batched hit lacks the path and hash issue_read needs", e)
		}
	}
	if strings.Join(matched["issue_both"], ",") != "deadlock,NullPointerException" || strings.Join(matched["issue_npe"], ",") != "NullPointerException" {
		t.Fatal("hits do not list the queries that found them", matched)
	}
	// One call, one audit line: the search trail Routing hands to assignment.
	audit, _ := os.ReadFile(s.Config.AuditPath)
	if strings.Count(string(audit), `"tool":"issue_search"`) != 1 || !strings.Contains(string(audit), `"queries"`) {
		t.Fatal("the batched search was not audited once with its queries", string(audit))
	}
}

func TestIssueSearchKeepsSingleQueryAndRejectsBadBatches(t *testing.T) {
	s := issueSearchFixture(t)
	single, err := s.Call(context.Background(), "issue_search", json.RawMessage(`{"query":"deadlock","limit":1}`))
	if err != nil || single["total"] != 2 || single["next_offset"] != 1 {
		t.Fatal("single-query paging changed", single, err)
	}
	for args, refusal := range map[string]string{
		`{"queries":["a","b","c","d","e","f"]}`:    "1..5",
		`{"queries":[]}`:                           "1..5",
		`{"queries":["deadlock"],"offset":10}`:     "no offset",
		`{"queries":["deadlock"," "]}`:             "non-empty",
		`{"query":"deadlock","queries":["other"]}`: "not both",
	} {
		if _, err := s.Call(context.Background(), "issue_search", json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s: %v", args, err)
		}
	}
}
