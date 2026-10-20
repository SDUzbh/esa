package replaytools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentsRenderAsCompactLinesWhoseIDsRebuildExactly(t *testing.T) {
	commit := "3402a8361b7347326b8a9fdb22300986fbbf9cd0"
	doc := Numbered(Document{Source: "git", Prefix: commit + ":streams/src/main/java/StreamThread.java:", First: 511,
		Lines: []string{"    static class Impl {", "", "        private final Sensor commitTimeSensor;"}})
	text := renderDocument(doc)
	lines := strings.Split(text, "\n")
	if lines[0] != "streams/src/main/java/StreamThread.java at commit 3402a8361b73, lines 511-513" ||
		!strings.Contains(lines[1], "StreamThread.java:<line>#<check>") || !strings.Contains(lines[1], doc.Lines[0].ID) {
		t.Fatalf("header:\n%s", text)
	}
	for i, line := range lines[2:] {
		token, code, ok := strings.Cut(line, " | ")
		if !ok || "StreamThread.java:"+token != doc.Lines[i].ID || code != doc.Lines[i].Text {
			t.Fatalf("line %d %q does not rebuild %s", i, line, doc.Lines[i].ID)
		}
	}
	run := Numbered(Document{Source: "replay", Prefix: "tool-1792423501048213:", First: 1, Lines: []string{"CONTROL=PASS"}})
	if text := renderDocument(run); !strings.HasPrefix(text, "replay output run-48213, lines 1-1\n") ||
		!strings.HasSuffix(text, "\n"+strings.TrimPrefix(run.Lines[0].ID, "run-48213:")+" | CONTROL=PASS") {
		t.Fatalf("run output:\n%s", text)
	}
	if text := renderDocument(NumberedDocument{Source: "git", Prefix: commit + ":A.java:", First: 9, Last: 8}); !strings.HasSuffix(text, "(no lines)") {
		t.Fatal(text)
	}
}

func TestSearchRendersHitsByFileWithoutHashes(t *testing.T) {
	hit := func(file string, line int, text string) map[string]any {
		return map[string]any{"file": file, "line": line, "text": text, "commit": "f10ef2720b03b247883db7e02afb767203176ea3", "file_hash": strings.Repeat("9", 64)}
	}
	text := renderSearch([]map[string]any{
		hit("clients/KafkaConsumer.java", 1029, "        long pollTimeout = coordinator.timeToNextPoll(now);"),
		hit("clients/KafkaConsumer.java", 1101, "    timeToNextPoll"),
		hit("clients/ConsumerCoordinator.java", 272, "    public long timeToNextPoll(long now) {"),
	}, true)
	want := "3 literal hits at commit f10ef2720b03 (limit reached; more exist, narrow the query). Read a hit with read_code to cite it.\n" +
		"clients/KafkaConsumer.java\n  1029: long pollTimeout = coordinator.timeToNextPoll(now);\n  1101: timeToNextPoll\n" +
		"clients/ConsumerCoordinator.java\n  272: public long timeToNextPoll(long now) {"
	if text != want {
		t.Fatalf("got\n%s\nwant\n%s", text, want)
	}
	if strings.Contains(renderSearch(nil, false), "limit") {
		t.Fatal("an empty search claimed a limit")
	}
}

func TestTheModelReceivesCompactTextOverMCP(t *testing.T) {
	profiles, _ := filepath.Abs("../../profiles")
	s := callFixture(t)
	s.Config.ProfilesRoot = profiles
	call := func(name, args string) string {
		var out strings.Builder
		in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}` + "\n"
		if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(out.String()), &response); err != nil || response.Result.IsError {
			t.Fatalf("%s: %v %s", name, err, out.String())
		}
		return response.Result.Content[0].Text
	}
	read := call("read_code", `{"path":"Code.java","first_line":1,"line_count":2}`)
	id := EvidenceID("git", "pinned:Code.java:", 2, "second")
	if !strings.HasPrefix(read, "Code.java at commit pinned, lines 1-2\n") || !strings.HasSuffix(read, "\n"+strings.TrimPrefix(id, "Code.java:")+" | second") {
		t.Fatalf("read_code text:\n%s", read)
	}
	search := call("source_search", `{"query":"second","limit":5}`)
	if !strings.HasPrefix(search, "1 literal hit at commit pinned.") || !strings.Contains(search, "\nCode.java\n  2: second") || strings.Contains(search, "file_hash") {
		t.Fatalf("source_search text:\n%s", search)
	}
	// Other tools keep JSON.
	if other := call("capability_request", `{"profile":"general","version":"1","requested_capabilities":["source"],"hypothesis_id":"H1","reason":"r","expected_observation":"o"}`); !strings.HasPrefix(other, "{") {
		t.Fatalf("capability_request is no longer JSON: %s", other)
	}
}
