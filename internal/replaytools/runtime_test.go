package replaytools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeStartsOnlyAfterApprovedProfileRequest(t *testing.T) {
	dir := t.TempDir()
	root, _ := filepath.Abs("../../profiles")
	helper := filepath.Join(dir, "helper.py")
	code := `import argparse,json,pathlib,sys
p=argparse.ArgumentParser()
for name in ['evidence','run-dir','profile']: p.add_argument('--'+name)
p.add_argument('--workspace',action='store_true')
a=p.parse_args(); d=pathlib.Path(a.run_dir)
(d/'workspace').write_text(str(a.workspace))
(d/'started').write_text(a.profile)
c={'container':'owned-fixture','session':'fixture','source_dir':str(d),'source_commit':'pinned','files':{'Example.java':'hash'},'audit_path':str(d/'audit.jsonl'),'readiness':{'prepared_profile':a.profile}}
(d/'config.json').write_text(json.dumps(c))
print(json.dumps({'tools_config':str(d/'config.json')}),flush=True)
sys.stdin.read()
(d/'cleaned').write_text('completed')
`
	if err := os.WriteFile(helper, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	s := Server{Config: Config{ProfilesRoot: root, AllowedProfiles: []string{"general"}, AvailableCapabilities: []string{"source", "runtime"}, SourceDir: dir, Commit: "pinned", Files: map[string]string{"Example.java": "hash"}, AuditPath: filepath.Join(dir, "audit.jsonl"), Runtime: &RuntimeSetup{Script: helper, Evidence: filepath.Join(dir, "packet.json"), Directory: dir, Workspace: true}}}
	_, err := s.Call(context.Background(), "capability_request", json.RawMessage(`{"profile":"outside-policy","version":"2","requested_capabilities":["runtime"],"hypothesis_id":"H1","reason":"debug","expected_observation":"trace"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "started")); !os.IsNotExist(err) {
		t.Fatal("denied Profile started a runtime")
	}
	result, err := s.Call(context.Background(), "capability_request", json.RawMessage(`{"profile":"general","version":"2","requested_capabilities":["runtime"],"hypothesis_id":"H1","reason":"debug","expected_observation":"trace"}`))
	if err != nil || s.Config.Container != "owned-fixture" || result["readiness"].(map[string]any)["prepared_profile"] != "general" {
		t.Fatal("approved Profile did not prepare its environment", result, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "workspace")); err != nil || string(b) != "True" {
		t.Fatal("generic mode did not reach helper", err)
	}
	if err = s.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "cleaned")); err != nil {
		t.Fatal("owned helper did not clean up", err)
	}
}
