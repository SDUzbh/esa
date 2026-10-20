package replaytools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rangesFixture registers two source files.
func rangesFixture(t *testing.T) Server {
	t.Helper()
	s := callFixture(t)
	source := []byte("one\ntwo\nthree\n")
	if err := os.WriteFile(filepath.Join(s.Config.SourceDir, "Other.java"), source, 0400); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(source)
	s.Config.Files["Other.java"] = hex.EncodeToString(sum[:])
	return s
}

func TestReadCodeReadsSeveralRangesInOneCall(t *testing.T) {
	s := rangesFixture(t)
	args := json.RawMessage(`{"ranges":[{"path":"Code.java","first_line":2,"line_count":1},{"path":"Other.java","first_line":1,"line_count":3}]}`)
	result, err := s.Call(context.Background(), "read_code", args)
	if err != nil {
		t.Fatal(err)
	}
	docs, ok := result["documents"].([]NumberedDocument)
	if !ok || len(docs) != 2 || docs[0].Lines[0].Text != "second" || len(docs[1].Lines) != 3 {
		t.Fatal("ranges were not each returned as a document", result)
	}
	// Each range is audited as an ordinary read, so evidence validation is unchanged.
	audited, _, err := ReadAudit(s.Config.AuditPath)
	if err != nil || len(audited) != 2 || audited[1].Prefix != "pinned:Other.java:" {
		t.Fatal("ranges were not audited as single reads", audited, err)
	}
	raw, _ := os.ReadFile(s.Config.AuditPath)
	if strings.Count(string(raw), `"tool":"read_code"`) != 2 || strings.Contains(string(raw), `"ranges"`) {
		t.Fatal("audit records do not look like single reads", string(raw))
	}
	for _, doc := range docs {
		for _, line := range doc.Lines {
			if !strings.Contains(renderResult("read_code", result), line.ID[strings.Index(line.ID, ":")+1:]+" | "+line.Text) {
				t.Fatal("a returned line is missing from the rendered text", line)
			}
		}
	}
	if blocks := strings.Split(renderResult("read_code", result), "\n\n"); len(blocks) != 2 || !strings.HasPrefix(blocks[1], "Other.java at commit pinned, lines 1-3") {
		t.Fatal("ranges were not rendered as separate blocks", blocks)
	}
}

func TestReadCodeRejectsBadBatchesWithoutAuditing(t *testing.T) {
	s := rangesFixture(t)
	for args, refusal := range map[string]string{
		`{"ranges":[]}`: "1..5",
		`{"ranges":[{"path":"Code.java","first_line":1,"line_count":1},{"path":"Code.java","first_line":1,"line_count":1},{"path":"Code.java","first_line":1,"line_count":1},{"path":"Code.java","first_line":1,"line_count":1},{"path":"Code.java","first_line":1,"line_count":1},{"path":"Code.java","first_line":1,"line_count":1}]}`: "1..5",
		`{"path":"Code.java","ranges":[{"path":"Code.java","first_line":1,"line_count":1}]}`:                                    "either",
		`{"ranges":[{"path":"Code.java","first_line":1,"line_count":1},{"path":"Missing.java","first_line":1,"line_count":1}]}`: "range 2 (Missing.java)",
	} {
		if _, err := s.Call(context.Background(), "read_code", json.RawMessage(args)); err == nil || !strings.Contains(err.Error(), refusal) {
			t.Errorf("%s: %v", args, err)
		}
	}
	if _, err := os.Stat(s.Config.AuditPath); !os.IsNotExist(err) {
		t.Fatal("a refused batch was partly audited")
	}
	if single, err := s.Call(context.Background(), "read_code", json.RawMessage(`{"path":"Code.java","first_line":1,"line_count":2}`)); err != nil || single["evidence"] == nil {
		t.Fatal("single-range reads changed", single, err)
	}
}
