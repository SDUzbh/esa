package replay_test

import (
	"encoding/json"
	"esa/internal/replay"
	"testing"
	"time"
)

func TestJiraReplayRewindsAndDropsFutureEvidence(t *testing.T) {
	at := time.Date(2015, 11, 24, 0, 13, 27, 387000000, time.UTC)
	envelope := map[string]any{
		"metadata": map[string]any{"history_complete": true, "history_reported_total": 2},
		"issue":    map[string]any{"key": "KAFKA-X", "fields": map[string]any{"summary": "Future title", "description": "Future solution", "issuetype": map[string]string{"name": "Bug"}, "status": map[string]string{"name": "Resolved"}, "created": at.Format(time.RFC3339Nano), "updated": at.Add(time.Hour).Format(time.RFC3339Nano), "versions": []any{map[string]string{"name": "future"}}, "fixVersions": []any{map[string]string{"name": "secret"}}}},
		"changelog": []any{
			map[string]any{"created": at.Add(time.Hour).Format(time.RFC3339Nano), "items": []any{map[string]any{"field": "description", "fromString": "Reported failure", "toString": "Future solution"}, map[string]any{"field": "summary", "fromString": "Original failure", "toString": "Future title"}, map[string]any{"field": "status", "fromString": "Open", "toString": "Resolved"}, map[string]any{"field": "Version", "fromString": "old", "toString": "future"}}},
			map[string]any{"created": at.Add(2 * time.Hour).Format(time.RFC3339Nano), "items": []any{map[string]any{"field": "Workflow", "fromString": "a", "toString": "b"}}},
		},
		"comments": []any{map[string]any{"id": "1", "body": "Edited secret", "created": at.Format(time.RFC3339Nano), "updated": at.Add(time.Hour).Format(time.RFC3339Nano)}, map[string]any{"id": "2", "body": "Future fix", "created": at.Add(time.Hour).Format(time.RFC3339Nano)}, map[string]any{"id": "3", "body": "Visible evidence", "created": at.Format(time.RFC3339Nano), "updated": at.Format(time.RFC3339Nano)}},
	}
	b, _ := json.Marshal(envelope)
	ticket, audit, err := replay.JiraAt(b, "1", at)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Subject != "Original failure" || ticket.Description != "Reported failure" || ticket.Status != "Open" || ticket.Closed || ticket.Versions[0] != "old" || len(ticket.Comments) != 1 || ticket.Comments[0].Body != "Visible evidence" || ticket.Resolution.Solution != "" || audit.ExcludedComments != 2 {
		t.Fatalf("future data leaked: %+v %+v", ticket, audit)
	}
	envelope["metadata"] = map[string]any{"history_complete": false, "history_reported_total": 2}
	b, _ = json.Marshal(envelope)
	if _, _, err = replay.JiraAt(b, "1", at); err == nil {
		t.Fatal("incomplete history accepted")
	}
}
