package replaytools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"esa/internal/capabilities"
)

type ToolPackage struct {
	Version string `json:"version"`
	File    string `json:"file,omitempty"`
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256"`
}

func has(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func decodeArgs(raw json.RawMessage, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("unexpected arguments")
	}
	return nil
}
func (s *Server) appendAudit(record Record) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.Config.AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func (s *Server) extendedCall(ctx context.Context, name string, args json.RawMessage) (map[string]any, error) {
	if s.Config.ProfilesRoot == "" {
		return nil, errors.New("extended capabilities are not registered")
	}
	result := map[string]any{}
	var err error
	switch name {
	case "capability_request":
		var request capabilities.Request
		if err = decodeArgs(args, &request); err != nil {
			return nil, err
		}
		planner := capabilities.Planner{
			Root:                  s.Config.ProfilesRoot,
			AllowedProfiles:       s.Config.AllowedProfiles,
			AvailableCapabilities: s.Config.AvailableCapabilities,
		}
		decision := planner.Resolve(request)
		if decision.Status == "granted" &&
			s.Config.ProfileHashes[request.Profile] != "" &&
			decision.Profile.ManifestHash != s.Config.ProfileHashes[request.Profile] {
			decision = capabilities.Decision{Status: "denied", Reason: "Profile manifest changed after the task catalog was frozen", Granted: []string{}}
		}
		if decision.Status == "granted" &&
			s.Config.Container == "" &&
			(has(decision.Granted, "runtime") || has(decision.Granted, "debugger") || has(decision.Granted, "temporary_tools")) {
			if err := s.prepareRuntime(ctx, request.Profile); err != nil {
				decision = capabilities.Decision{Status: "denied", Reason: "Registered environment failed: " + err.Error(), Granted: []string{}}
			}
		}
		result["decision"] = decision
		catalog := []map[string]string{}
		ids := []string{}
		for id := range s.Config.Tools {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			pkg := s.Config.Tools[id]
			catalog = append(catalog, map[string]string{"tool_id": id, "version": pkg.Version, "sha256": pkg.SHA256})
		}
		result["tool_catalog"] = catalog
		result["readiness"] = s.Config.Readiness
		if decision.Status == "granted" {
			for _, grant := range decision.Granted {
				if !has(s.granted, grant) {
					s.granted = append(s.granted, grant)
				}
			}
		}
	case "source_search":
		var request struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err = decodeArgs(args, &request); err != nil {
			return nil, err
		}
		if len(request.Query) < 2 || len(request.Query) > 256 || request.Limit < 1 || request.Limit > 50 {
			return nil, errors.New("literal query must contain 2..256 bytes and limit 1..50")
		}
		result, err = s.search(request.Query, request.Limit)
	case "tool_install":
		var request struct {
			ID          string `json:"tool_id"`
			Version     string `json:"version"`
			Reason      string `json:"reason"`
			Hypothesis  string `json:"hypothesis_id"`
			Observation string `json:"expected_observation"`
		}
		if err = decodeArgs(args, &request); err != nil {
			return nil, err
		}
		if !has(s.granted, "temporary_tools") {
			result = map[string]any{"status": "denied", "reason": "temporary_tools capability has not been granted"}
			break
		}
		pkg, ok := s.Config.Tools[request.ID]
		if !ok ||
			pkg.Version == "" ||
			pkg.Version != request.Version ||
			strings.TrimSpace(request.Reason) == "" ||
			request.Hypothesis == "" ||
			request.Observation == "" ||
			filepath.Base(request.ID) != request.ID ||
			strings.ContainsAny(request.ID, "/\\.") ||
			strings.ContainsAny(pkg.Version, "/\\") {
			result = map[string]any{"status": "denied", "reason": "tool/version/purpose is outside the registered catalog"}
			break
		}
		result, err = s.installTool(ctx, request.ID, pkg)
	case "maven_install":
		var request struct {
			Coordinate  string `json:"coordinate"`
			Reason      string `json:"reason"`
			Hypothesis  string `json:"hypothesis_id"`
			Observation string `json:"expected_observation"`
		}
		if err = decodeArgs(args, &request); err != nil {
			return nil, err
		}
		_, _, _, valid := mavenCoordinate(request.Coordinate)
		switch {
		case s.Config.Maven == nil:
			result = map[string]any{"status": "denied", "reason": "Maven downloads are not enabled for this task"}
		case !has(s.granted, "temporary_tools"):
			result = map[string]any{"status": "denied", "reason": "temporary_tools capability has not been granted"}
		case !valid || strings.TrimSpace(request.Reason) == "" || request.Hypothesis == "" || request.Observation == "":
			result = map[string]any{"status": "denied", "reason": "a group:artifact:version coordinate, reason, hypothesis and expected observation are required"}
		default:
			result, err = s.installMaven(ctx, request.Coordinate)
		}
	default:
		return nil, errors.New("tool is not registered")
	}
	if err != nil {
		result = map[string]any{"status": "failed", "reason": err.Error()}
	}
	if auditErr := s.appendAudit(Record{Tool: name, Arguments: args, Result: result}); auditErr != nil {
		return nil, auditErr
	}
	return result, err
}
func (s *Server) search(query string, limit int) (map[string]any, error) {
	paths := []string{}
	for path := range s.Config.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > 20000 {
		return nil, errors.New("source manifest exceeds bounded search policy")
	}
	root, err := os.OpenRoot(s.Config.SourceDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	hits := []map[string]any{}
	var scanned int64
	for _, path := range paths {
		f, err := root.Open(path)
		if err != nil {
			return nil, err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, 4<<20+1))
		f.Close()
		if readErr != nil {
			return nil, readErr
		}
		scanned += int64(len(b))
		if len(b) > 4<<20 || scanned > 64<<20 {
			return nil, errors.New("source search byte limit exceeded; narrow the registered manifest")
		}
		if capabilities.Digest(b) != s.Config.Files[path] {
			return nil, errors.New("historical source hash changed")
		}
		for index, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, query) {
				if len(line) > 2048 {
					line = line[:2048]
				}
				hits = append(hits, map[string]any{"file": path, "line": index + 1, "text": line, "commit": s.Config.Commit, "file_hash": s.Config.Files[path]})
				if len(hits) == limit {
					return map[string]any{"kind": "literal_text", "hits": hits, "truncated": true}, nil
				}
			}
		}
	}
	return map[string]any{"kind": "literal_text", "hits": hits, "truncated": false}, nil
}
func (s *Server) installTool(ctx context.Context, id string, pkg ToolPackage) (map[string]any, error) {
	call, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := s.owned(call); err != nil {
		return nil, err
	}
	if len(pkg.SHA256) != 64 {
		return nil, errors.New("tool must have a pinned SHA256")
	}
	dest := "/workspace/tools/" + id + "-" + capabilities.Digest([]byte(pkg.Version))[:12] + "/tool"
	// An installed file is reusable only if its executable bytes still match.
	if s.installed(call, dest, pkg.SHA256) {
		return map[string]any{"status": "installed", "cache_hit": true, "path": dest, "version": pkg.Version, "sha256": pkg.SHA256}, nil
	}
	var reader io.ReadCloser
	if pkg.File != "" {
		if !filepath.IsAbs(pkg.File) {
			return nil, errors.New("catalog tool file must be absolute")
		}
		f, err := os.Open(pkg.File)
		if err != nil {
			return nil, err
		}
		reader = f
	} else {
		if !strings.HasPrefix(pkg.URL, "https://") {
			return nil, errors.New("catalog downloads require pinned HTTPS URLs")
		}
		request, err := http.NewRequestWithContext(call, http.MethodGet, pkg.URL, nil)
		if err != nil {
			return nil, err
		}
		client := &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("catalog redirects are not permitted")
		}}
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("catalog download failed")
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return nil, fmt.Errorf("catalog download returned HTTP %d", response.StatusCode)
		}
		reader = response.Body
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, 16<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 16<<20 || capabilities.Digest(data) != pkg.SHA256 {
		return nil, errors.New("tool package size or SHA256 validation failed")
	}
	return s.placeTool(call, dest, data, pkg.SHA256, pkg.Version)
}

func (s *Server) installed(ctx context.Context, dest, digest string) bool {
	out, err := s.docker(ctx, "exec", s.Config.Container, "sha256sum", dest)
	return err == nil && len(strings.Fields(string(out))) > 0 && strings.Fields(string(out))[0] == digest
}

// placeTool streams verified bytes into the sandbox's scratch tools directory
// and checks the installed digest.
func (s *Server) placeTool(call context.Context, dest string, data []byte, digest, version string) (map[string]any, error) {
	if s.installed(call, dest, digest) {
		return map[string]any{"status": "installed", "cache_hit": true, "path": dest, "version": version, "sha256": digest}, nil
	}
	if output, err := s.docker(call, "exec", s.Config.Container, "mkdir", "-p", filepath.Dir(dest)); err != nil {
		return nil, fmt.Errorf("tool directory preparation failed: %s", strings.TrimSpace(string(output)))
	}
	// Docker's archive API cannot reliably copy into a tmpfs with a read-only
	// root filesystem. Stream approved bytes to a fixed scratch path as UID 1000.
	base := []string{}
	if s.Config.DockerContext != "" {
		base = append(base, "--context", s.Config.DockerContext)
	}
	base = append(base, "exec", "-i", s.Config.Container, "sh", "-c", `rm -f -- "$1"; umask 077; set -C; cat > "$1"; chmod 0555 "$1"`, "installer", dest)
	cmd := exec.CommandContext(call, "docker", base...)
	cmd.Stdin = bytes.NewReader(data)
	var output limitedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sandbox tool installation failed: %s", strings.TrimSpace(output.String()))
	}
	if !s.installed(call, dest, digest) {
		return nil, errors.New("installed tool hash verification failed")
	}
	return map[string]any{"status": "installed", "cache_hit": false, "path": dest, "version": version, "sha256": digest}, nil
}
func extendedTools() []map[string]any {
	str := map[string]string{"type": "string"}
	integer := map[string]string{"type": "integer"}
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []map[string]any{
		{
			"name":        "source_search",
			"description": "Bounded literal search over hash-validated frozen source. Hits are not parser-verified symbols; inspect read_code for evidence.",
			"inputSchema": schema(map[string]any{"query": str, "limit": integer}, "query", "limit"),
		},
		{
			"name": "capability_request",
			"description": "Automatically select an approved Profile and request task capabilities for a hypothesis. " +
				"Returns a typed grant or denial and the approved Skill; cannot expand host policy.",
			"inputSchema": schema(
				map[string]any{
					"profile":                str,
					"version":                str,
					"requested_capabilities": map[string]any{"type": "array", "items": str},
					"hypothesis_id":          str,
					"reason":                 str,
					"expected_observation":   str,
				},
				"profile",
				"version",
				"requested_capabilities",
				"hypothesis_id",
				"reason",
				"expected_observation",
			),
		},
		{
			"name": "tool_install",
			"description": "Install one operator-catalogued pinned tool into the owned scratch tools directory. " +
				"Requires a granted temporary_tools capability. Never replaces product inputs.",
			"inputSchema": schema(
				map[string]any{"tool_id": str, "version": str, "reason": str, "hypothesis_id": str, "expected_observation": str},
				"tool_id",
				"version",
				"reason",
				"hypothesis_id",
				"expected_observation",
			),
		},
	}
}
