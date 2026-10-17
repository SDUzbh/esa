package replaytools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	result, err := s.Call(context.Background(), "read_code", json.RawMessage(`{"path":"Code.java","first_line":2,"line_count":175}`))
	if err != nil {
		t.Fatal(err)
	}
	view := result["evidence"].(NumberedDocument)
	if view.First != 2 || view.Last != 2 || len(view.Lines) != 1 || view.Lines[0].Line != 2 || view.Lines[0].ID != EvidenceID("git", "historical:Code.java:", 2, "complete(response);") {
		t.Fatal("absolute lines or evidence IDs lost at EOF", view)
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
	if err != nil || len(docs) != 2 || docs[0].First != 2 || len(requests) != 1 || requests[0].State != "requested" {
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

// The isolation policy must hold for every task container, including sessions
// without Profiles, where it used to be skipped.
func TestOwnedEnforcesIsolationWithoutProfiles(t *testing.T) {
	bin := t.TempDir()
	inspect := filepath.Join(bin, "inspect.json")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = --format ] && { echo fixture; exit 0; }; done\ncat " + inspect + "\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	compliant := `{"Config":{"User":"1000:1000"},"HostConfig":{"ReadonlyRootfs":true,"NetworkMode":"none","CapDrop":["ALL"],"Memory":1,"NanoCpus":1,"PidsLimit":1},"Mounts":[{"Source":"v","Destination":"/input","RW":false}]}`
	for name, tc := range map[string]struct {
		container string
		ok        bool
	}{
		"compliant":        {compliant, true},
		"writable root":    {strings.Replace(compliant, `"ReadonlyRootfs":true`, `"ReadonlyRootfs":false`, 1), false},
		"network":          {strings.Replace(compliant, `"NetworkMode":"none"`, `"NetworkMode":"bridge"`, 1), false},
		"root user":        {strings.Replace(compliant, `"User":"1000:1000"`, `"User":"0:0"`, 1), false},
		"capabilities":     {strings.Replace(compliant, `["ALL"]`, `[]`, 1), false},
		"no memory limit":  {strings.Replace(compliant, `"Memory":1`, `"Memory":0`, 1), false},
		"writable input":   {strings.Replace(compliant, `"RW":false`, `"RW":true`, 1), false},
		"docker socket":    {strings.Replace(compliant, `"Source":"v"`, `"Source":"/var/run/docker.sock"`, 1), false},
		"inspection fails": {`not json`, false},
	} {
		if err := os.WriteFile(inspect, []byte("["+tc.container+"]"), 0600); err != nil {
			t.Fatal(err)
		}
		s := Server{Config: Config{Container: "task", Session: "fixture"}}
		if err := s.owned(context.Background()); (err == nil) != tc.ok {
			t.Errorf("%s: owned() = %v, want ok=%v", name, err, tc.ok)
		}
	}
	s := Server{Config: Config{Container: "task", Session: "another-session"}}
	if err := s.owned(context.Background()); err == nil {
		t.Error("a container labelled for another session was accepted")
	}
}
