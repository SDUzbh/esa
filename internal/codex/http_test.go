package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeLogsAuthenticationPaginationAndPartialEvents(t *testing.T) {
	p := &Provider{RunsDir: t.TempDir()}
	token := strings.Repeat("x", 24)
	handler := p.Handler(token)
	get := func(path, credential string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", credential)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := get("/runs/run_example/logs?stage=route", ""); w.Code != 401 {
		t.Fatal("private runtime log did not require authentication", w.Code)
	}
	for _, path := range []string{"/runs/run_../logs?stage=route", "/runs/run_example/logs?stage=../../stderr", "/runs/run_example/logs?stage=route&offset=-1"} {
		if w := get(path, "Bearer "+token); w.Code != 400 {
			t.Fatal("invalid path accepted", path, w.Code)
		}
	}
	page := func(stage string, offset int64) struct {
		Events     []json.RawMessage `json:"events"`
		Available  bool              `json:"available"`
		HasMore    bool              `json:"has_more"`
		NextOffset int64             `json:"next_offset"`
	} {
		t.Helper()
		w := get(fmt.Sprintf("/runs/run_example/logs?stage=%s&offset=%d", stage, offset), "Bearer "+token)
		var result struct {
			Events     []json.RawMessage `json:"events"`
			Available  bool              `json:"available"`
			HasMore    bool              `json:"has_more"`
			NextOffset int64             `json:"next_offset"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("invalid runtime page", w.Code, w.Body.String())
		}
		return result
	}
	if result := page("engineer", 0); result.Available || len(result.Events) != 0 {
		t.Fatal("uninvoked engineering was represented as an event stream")
	}
	dir := filepath.Join(p.RunsDir, "run_example", "routing")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var data strings.Builder
	for i := 0; i < 201; i++ {
		fmt.Fprintf(&data, "{\"type\":\"item.completed\",\"sequence\":%d}\n", i)
	}
	completeBytes := data.Len()
	data.WriteString(`{"type":"turn.completed"`)
	log := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(log, []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	first := page("route", 0)
	second := page("route", first.NextOffset)
	if len(first.Events) != 200 || !first.HasMore || len(second.Events) != 1 || second.HasMore || second.NextOffset != int64(completeBytes) {
		t.Fatal("events skipped/duplicated or partial cursor advanced", first, second)
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("}\n")
	f.Close()
	last := page("route", second.NextOffset)
	if len(last.Events) != 1 || string(last.Events[0]) != `{"type":"turn.completed"}` {
		t.Fatal("partial event was not resumed exactly", last)
	}
	if w := get("/runs/run_example/logs?stage=route&offset=1", "Bearer "+token); w.Code != 400 {
		t.Fatal("cursor inside an event accepted")
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "events.jsonl"), []byte("{\"private\":true}\n"), 0600)
	os.MkdirAll(filepath.Join(p.RunsDir, "run_escape"), 0700)
	if err := os.Symlink(outside, filepath.Join(p.RunsDir, "run_escape", "routing")); err != nil {
		t.Fatal(err)
	}
	if w := get("/runs/run_escape/logs?stage=route", "Bearer "+token); w.Code == 200 || strings.Contains(w.Body.String(), `"private"`) {
		t.Fatal("log symlink escaped provider root")
	}
}

func TestRuntimeLogsExposeActualPipelineEvents(t *testing.T) {
	p, in, _ := routingFixture(t, "analyze")
	// Each CLI fixture emits cache hits; the real adapter must carry them through
	// all stages and the authenticated API must return the original usage event.
	in.Budget.MaxInputTokens, in.Budget.MaxOutputTokens, in.Budget.WarnTotalTokens = 16, 25, 36
	report, err := p.Analyze(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.InputTokens != 36 || report.Usage.CachedInputTokens != 24 || len(report.Alerts) != 1 || report.Alerts[0].Code != "token_total_high" || !strings.Contains(report.Alerts[0].Message, "38.40") {
		t.Fatal("weighted pipeline accounting/alerts incorrect", report.Usage, report.Alerts)
	}
	for _, stage := range []string{"route", "engineer", "assign"} {
		r := httptest.NewRequest("GET", "/runs/"+in.RunID+"/logs?stage="+stage, nil)
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		p.Handler("test-token").ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"turn.completed"`) || !strings.Contains(w.Body.String(), `"input_tokens":12`) {
			t.Fatal("original provider usage event absent", stage, w.Code, w.Body.String())
		}
	}
}
