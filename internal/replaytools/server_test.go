package replaytools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFrozenSourceAndRequests(t *testing.T) {
	dir := t.TempDir()
	source := []byte("first\ncomplete(response);\n")
	if err := os.WriteFile(filepath.Join(dir, "Code.java"), source, 0400); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(source)
	s := Server{Config: Config{SourceDir: dir, Commit: "historical", Files: map[string]string{"Code.java": hex.EncodeToString(h[:])}, AuditPath: filepath.Join(dir, "audit.jsonl")}}
	call := func(name, args string) error {
		_, err := s.Call(context.Background(), name, json.RawMessage(args))
		return err
	}
	if err := call("read_code", `{"path":"Code.java","first_line":2,"line_count":1}`); err != nil {
		t.Fatal(err)
	}
	if call("read_code", `{"path":"../secret","first_line":1,"line_count":1}`) == nil {
		t.Fatal("path escape accepted")
	}
	if call("edit_code", `{}`) == nil {
		t.Fatal("source editing tool exposed")
	}
	if err := call("request_environment_change", `{"reason":"Wrong JDK","proposed_change":"Use the registered Java 8 runtime","validation":"Run the control test"}`); err != nil {
		t.Fatal(err)
	}
	docs, requests, err := ReadAudit(s.Config.AuditPath)
	if err != nil || len(docs) != 1 || docs[0].First != 2 || len(requests) != 1 || requests[0].State != "requested" {
		t.Fatalf("invalid audit: %+v %+v %v", docs, requests, err)
	}
	if err := os.Chmod(filepath.Join(dir, "Code.java"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Code.java"), []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if call("read_code", `{"path":"Code.java","first_line":1,"line_count":1}`) == nil {
		t.Fatal("changed source accepted")
	}
}

func TestMCPStdioHandshakeAndDiscovery(t *testing.T) {
	input := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-03-26\"}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n")
	var output bytes.Buffer
	if err := (&Server{}).Serve(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var a, b map[string]any
	if decoder.Decode(&a) != nil || decoder.Decode(&b) != nil {
		t.Fatal("invalid MCP response")
	}
	if a["result"].(map[string]any)["protocolVersion"] != "2025-03-26" || len(b["result"].(map[string]any)["tools"].([]any)) != 4 {
		t.Fatal("incorrect tool surface")
	}
}
