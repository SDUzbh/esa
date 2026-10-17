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

// callFixture is a source-only session over one registered file.
func callFixture(t *testing.T) Server {
	t.Helper()
	dir := t.TempDir()
	source := []byte("first\nsecond\n")
	if err := os.WriteFile(filepath.Join(dir, "Code.java"), source, 0400); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(source)
	return Server{Config: Config{Session: "s", SourceDir: dir, Commit: "pinned", Files: map[string]string{"Code.java": hex.EncodeToString(sum[:])}, AuditPath: filepath.Join(dir, "audit.jsonl")}}
}

func TestCallRejectsOutOfPolicyRequests(t *testing.T) {
	profiles, _ := filepath.Abs("../../profiles")
	for name, tc := range map[string]struct {
		setup func(*Server)
		tool  string
		args  string
		want  string
	}{
		"engineering asks for the Issue catalogue": {nil, "issue_search", `{}`, "belongs to Routing Agent"},
		"routing asks for source":                  {func(s *Server) { s.Config.IssuesOnly = true }, "read_code", `{}`, "Routing Agent cannot access"},
		"extended tool without Profiles":           {nil, "source_search", `{"query":"first","limit":1}`, "not registered"},
		"unknown argument":                         {nil, "read_code", `{"path":"Code.java","first_line":1,"line_count":1,"extra":1}`, "unknown field"},
		"unregistered path":                        {nil, "read_code", `{"path":"Other.java","first_line":1,"line_count":1}`, "registered source path"},
		"no lines":                                 {nil, "read_code", `{"path":"Code.java","first_line":1,"line_count":0}`, "1..200 lines"},
		"too many lines":                           {nil, "read_code", `{"path":"Code.java","first_line":1,"line_count":201}`, "1..200 lines"},
		"range past the end":                       {nil, "read_code", `{"path":"Code.java","first_line":9,"line_count":1}`, "range is empty"},
		"changed source":                           {func(s *Server) { s.Config.Files["Code.java"] = strings.Repeat("0", 64) }, "read_code", `{"path":"Code.java","first_line":1,"line_count":1}`, "hash changed"},
		"runtime without a grant":                  {func(s *Server) { s.Config.ProfilesRoot = profiles }, "run", `{"script":"true"}`, "request the runtime capability"},
		"oversized script":                         {nil, "run", `{"script":"` + strings.Repeat("x", 32769) + `"}`, "too large"},
		"search query too short":                   {func(s *Server) { s.Config.ProfilesRoot = profiles }, "source_search", `{"query":"x","limit":1}`, "2..256 bytes"},
		"search limit too large":                   {func(s *Server) { s.Config.ProfilesRoot = profiles }, "source_search", `{"query":"first","limit":51}`, "limit 1..50"},
	} {
		s := callFixture(t)
		if tc.setup != nil {
			tc.setup(&s)
		}
		if _, err := s.Call(context.Background(), tc.tool, json.RawMessage(tc.args)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func TestToolInstallStaysInsideTheGrantAndCatalog(t *testing.T) {
	profiles, _ := filepath.Abs("../../profiles")
	s := callFixture(t)
	s.Config.ProfilesRoot = profiles
	s.Config.Tools = map[string]ToolPackage{"jq": {Version: "1.7", SHA256: strings.Repeat("a", 64)}}
	request := `{"tool_id":"jq","version":"1.7","reason":"inspect JSON","hypothesis_id":"H1","expected_observation":"keys"}`
	result, err := s.Call(context.Background(), "tool_install", json.RawMessage(request))
	if err != nil || result["status"] != "denied" || !strings.Contains(result["reason"].(string), "temporary_tools") {
		t.Fatal("a tool was installable without the temporary_tools grant", result, err)
	}
	s.granted = []string{"temporary_tools"}
	for _, outside := range []string{
		`{"tool_id":"jq","version":"1.6","reason":"inspect JSON","hypothesis_id":"H1","expected_observation":"keys"}`,
		`{"tool_id":"curl","version":"1.7","reason":"inspect JSON","hypothesis_id":"H1","expected_observation":"keys"}`,
		`{"tool_id":"jq","version":"1.7","reason":" ","hypothesis_id":"H1","expected_observation":"keys"}`,
		`{"tool_id":"jq","version":"1.7","reason":"inspect JSON","hypothesis_id":"","expected_observation":"keys"}`,
	} {
		result, err = s.Call(context.Background(), "tool_install", json.RawMessage(outside))
		if err != nil || result["status"] != "denied" || !strings.Contains(result["reason"].(string), "outside the registered catalog") {
			t.Errorf("request %s was not denied: %v %v", outside, result, err)
		}
	}
	audit, err := os.ReadFile(s.Config.AuditPath)
	if err != nil || strings.Count(string(audit), `"tool":"tool_install"`) != 5 {
		t.Fatal("denied installs were not audited", err)
	}
}

// installDocker is a fake docker for a compliant task container: exec -i
// stores the streamed bytes and sha256sum reports their digest.
func installDocker(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	inspect := `[{"Config":{"User":"1000:1000"},"HostConfig":{"ReadonlyRootfs":true,"NetworkMode":"none","CapDrop":["ALL"],"Memory":1,"NanoCpus":1,"PidsLimit":1},"Mounts":[]}]`
	if err := os.WriteFile(filepath.Join(bin, "inspect.json"), []byte(inspect), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
d=` + bin + `
case " $* " in
*" --format "*) echo s ;;
*" inspect "*) cat "$d/inspect.json" ;;
*" exec -i "*) cat > "$d/stored" ;;
*" sha256sum "*) [ -f "$d/stored" ] && python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest(), "tool")' "$d/stored" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

func TestToolInstallVerifiesThePinnedPackage(t *testing.T) {
	bin := installDocker(t)
	tool := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("#!/bin/sh\necho ok\n"))
	pinned := hex.EncodeToString(sum[:])
	s := callFixture(t)
	s.Config.Container = "task"
	for name, tc := range map[string]struct {
		pkg  ToolPackage
		want string
	}{
		"unpinned":         {ToolPackage{Version: "1", File: tool, SHA256: "abc"}, "pinned SHA256"},
		"relative file":    {ToolPackage{Version: "1", File: "tool", SHA256: pinned}, "must be absolute"},
		"plain HTTP":       {ToolPackage{Version: "1", URL: "http://example.invalid/tool", SHA256: pinned}, "pinned HTTPS"},
		"different digest": {ToolPackage{Version: "1", File: tool, SHA256: strings.Repeat("b", 64)}, "SHA256 validation failed"},
	} {
		if _, err := s.installTool(context.Background(), "jq", tc.pkg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
	pkg := ToolPackage{Version: "1", File: tool, SHA256: pinned}
	result, err := s.installTool(context.Background(), "jq", pkg)
	if err != nil || result["status"] != "installed" || result["cache_hit"] != false {
		t.Fatal("a pinned package was not installed", result, err)
	}
	if stored, _ := os.ReadFile(filepath.Join(bin, "stored")); string(stored) != "#!/bin/sh\necho ok\n" {
		t.Fatal("the sandbox did not receive the approved bytes")
	}
	if result, err = s.installTool(context.Background(), "jq", pkg); err != nil || result["cache_hit"] != true {
		t.Fatal("an installed package with matching bytes was not reused", result, err)
	}
}
