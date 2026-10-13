package replaytools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"esa/internal/capabilities"
)

func TestExtendedGatewayEnforcesPolicyAndFrozenSearch(t *testing.T) {
	dir := t.TempDir()
	root, _ := filepath.Abs("../../profiles")
	content := []byte("class Example {\n  assignment.size();\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "Example.java"), content, 0400); err != nil {
		t.Fatal(err)
	}
	s := Server{Config: Config{ProfilesRoot: root, AllowedProfiles: []string{"general", "java-debug"}, AvailableCapabilities: []string{"source"}, SourceDir: dir, Commit: "pinned", Files: map[string]string{"Example.java": capabilities.Digest(content)}, AuditPath: filepath.Join(dir, "audit.jsonl")}}
	call := func(name, args string) (map[string]any, error) {
		return s.Call(context.Background(), name, json.RawMessage(args))
	}
	result, err := call("source_search", `{"query":"assignment","limit":5}`)
	if err != nil || len(result["hits"].([]map[string]any)) != 1 || result["kind"] != "literal_text" {
		t.Fatalf("search result: %+v %v", result, err)
	}
	result, err = call("capability_request", `{"profile":"java-debug","version":"2","requested_capabilities":["source"],"hypothesis_id":"H1","reason":"Inspect assignment","expected_observation":"Find dereference"}`)
	if err != nil || result["decision"].(capabilities.Decision).Status != "granted" {
		t.Fatalf("grant: %+v %v", result, err)
	}
	if _, err = call("run", `{"script":"touch /workspace/bypass"}`); err == nil {
		t.Fatal("run bypassed denied runtime")
	}
	result, err = call("tool_install", `{"tool_id":"unknown","version":"2","hypothesis_id":"H1","reason":"debug","expected_observation":"trace"}`)
	if err != nil || result["status"] != "denied" {
		t.Fatalf("installation policy bypassed: %+v %v", result, err)
	}
	plan, err := ReadPlan(s.Config.AuditPath)
	if err != nil || plan == nil || plan.Profile != "java-debug" || plan.SkillHash == "" {
		t.Fatalf("missing plan audit: %+v %v", plan, err)
	}
	if err = os.Chmod(filepath.Join(dir, "Example.java"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "Example.java"), []byte("future repair\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = call("source_search", `{"query":"repair","limit":5}`); err == nil {
		t.Fatal("future/tampered source entered search")
	}
}
