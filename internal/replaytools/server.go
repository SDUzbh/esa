// Package replaytools exposes only an owned replay container and frozen source.
package replaytools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"esa/internal/esa"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Document struct {
	Source string   `json:"source"`
	Prefix string   `json:"reference_prefix"`
	First  int      `json:"first_line"`
	Lines  []string `json:"lines"`
}

// Config is an operator-owned registration of frozen source, scope and available tools.
// Ticket text cannot grant capabilities or introduce arbitrary host paths.
type Config struct {
	IssueContext          string                 `json:"issue_context,omitempty"`
	IssuesRoot            string                 `json:"issues_root,omitempty"`
	IssueDirectory        string                 `json:"issue_directory,omitempty"`
	IssueDirectoryHash    string                 `json:"issue_directory_hash,omitempty"`
	IssuesOnly            bool                   `json:"issues_only,omitempty"`
	Runtime               *RuntimeSetup          `json:"runtime_setup,omitempty"`
	DockerContext         string                 `json:"docker_context"`
	Container             string                 `json:"container"`
	Session               string                 `json:"session"`
	SourceDir             string                 `json:"source_dir"`
	Commit                string                 `json:"source_commit"`
	Files                 map[string]string      `json:"files"`
	AuditPath             string                 `json:"audit_path"`
	ProfilesRoot          string                 `json:"profiles_root,omitempty"`
	AllowedProfiles       []string               `json:"allowed_profiles,omitempty"`
	AvailableCapabilities []string               `json:"available_capabilities,omitempty"`
	Tools                 map[string]ToolPackage `json:"tools,omitempty"`
	Maven                 *MavenPolicy           `json:"maven,omitempty"`
	ProfileHashes         map[string]string      `json:"profile_hashes,omitempty"`
	Readiness             map[string]any         `json:"readiness,omitempty"`
}
type Record struct {
	Tool      string                  `json:"tool"`
	Arguments json.RawMessage         `json:"arguments"`
	Document  *Document               `json:"document,omitempty"`
	Request   *esa.EnvironmentRequest `json:"request,omitempty"`
	Result    map[string]any          `json:"result,omitempty"`
}

var Names = []string{
	"read_code", "run", "install_environment", "request_environment_change", "source_search", "capability_request", "tool_install", "maven_install",
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.IssuesOnly {
		if c.Session == "" || !filepath.IsAbs(c.AuditPath) {
			return c, errors.New("invalid routing tool registration")
		}
		_, err := esa.OpenIssueDirectory(c.IssuesRoot, c.IssueDirectory, c.IssueDirectoryHash)
		return c, err
	}
	if c.Container == "" && c.Runtime == nil && !(c.ProfilesRoot != "" && len(c.AvailableCapabilities) == 1 && c.AvailableCapabilities[0] == "source") ||
		c.Session == "" ||
		c.Commit == "" ||
		len(c.Files) == 0 ||
		!filepath.IsAbs(c.SourceDir) ||
		!filepath.IsAbs(c.AuditPath) {
		return c, errors.New("invalid replay tool registration")
	}
	if c.Runtime != nil && (!filepath.IsAbs(c.Runtime.Script) || !filepath.IsAbs(c.Runtime.Evidence) || !filepath.IsAbs(c.Runtime.Directory)) {
		return c, errors.New("runtime setup must use operator-owned absolute paths")
	}
	return c, nil
}

func ReadAudit(path string) ([]Document, []esa.EnvironmentRequest, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var docs []Document
	var requests []esa.EnvironmentRequest
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var r Record
		if err = json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return nil, nil, err
		}
		if r.Document != nil {
			docs = append(docs, *r.Document)
		}
		if r.Request != nil {
			requests = append(requests, *r.Request)
		}
	}
	return docs, requests, scanner.Err()
}

// Server exposes the permitted MCP surface and records evidence-producing tool calls.
// Routing-only sessions cannot access engineering source/runtime tools, and vice versa.
type Server struct {
	issueDirectory *esa.IssueDirectory
	Config         Config
	granted        []string
	cleanup        func() error
}

// RuntimeSetup registers the operator helper and its owned task directory.
// Workspace selects an empty build environment rather than a supplied scenario.
type RuntimeSetup struct {
	Script    string `json:"script"`
	Evidence  string `json:"evidence"`
	Directory string `json:"directory"`
	Workspace bool   `json:"workspace,omitempty"`
}

func (s *Server) docker(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{}
	if s.Config.DockerContext != "" {
		base = append(base, "--context", s.Config.DockerContext)
	}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if left := 65536 - b.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
func (s *Server) owned(ctx context.Context) error {
	b, err := s.docker(ctx, "inspect", "--format", "{{index .Config.Labels \"esa.replay.session\"}}", s.Config.Container)
	if err != nil || strings.TrimSpace(string(b)) != s.Config.Session {
		return errors.New("replay container ownership check failed")
	}
	// The isolation policy applies to every task container, with or without
	// Profiles: read-only root, no network, uid 1000, no capabilities, resource
	// limits, a read-only /input and no Docker socket.
	b, err = s.docker(ctx, "inspect", s.Config.Container)
	var info []struct {
		Config     struct{ User string }
		HostConfig struct {
			ReadonlyRootfs bool
			NetworkMode    string
			CapDrop        []string
			Memory         int64
			NanoCpus       int64
			PidsLimit      int64
			SecurityOpt    []string
		}
		Mounts []struct {
			Source, Destination string
			RW                  bool
		}
	}
	if err != nil || json.Unmarshal(b, &info) != nil || len(info) != 1 {
		return errors.New("sandbox readiness inspection failed")
	}
	c := info[0]
	if !c.HostConfig.ReadonlyRootfs ||
		c.HostConfig.NetworkMode != "none" ||
		c.Config.User != "1000:1000" ||
		!has(c.HostConfig.CapDrop, "ALL") ||
		c.HostConfig.Memory <= 0 ||
		c.HostConfig.NanoCpus <= 0 ||
		c.HostConfig.PidsLimit <= 0 {
		return errors.New("sandbox does not satisfy task isolation policy")
	}
	for _, mount := range c.Mounts {
		if strings.Contains(mount.Source, "docker.sock") || strings.Contains(mount.Destination, "docker.sock") || mount.Destination == "/input" && mount.RW {
			return errors.New("sandbox exposes forbidden writable product input or host socket")
		}
	}
	return nil
}

// Call checks the session role and capability policy before invoking a named tool.
// Results and inspected documents enter the audit used by report validation.
func (s *Server) Call(ctx context.Context, name string, args json.RawMessage) (map[string]any, error) {
	if name == "issue_search" || name == "issue_read" || name == "issue_context" {
		if !s.Config.IssuesOnly {
			return nil, errors.New("Issue catalogue access belongs to Routing Agent")
		}
		return s.issueCall(name, args)
	}
	if s.Config.IssuesOnly {
		return nil, errors.New("Routing Agent cannot access source, runtime or installation tools")
	}
	if name == "source_search" || name == "capability_request" || name == "tool_install" || name == "maven_install" {
		return s.extendedCall(ctx, name, args)
	}
	var a struct {
		Path       string      `json:"path"`
		First      int         `json:"first_line"`
		Count      int         `json:"line_count"`
		Ranges     []readRange `json:"ranges"`
		Script     string      `json:"script"`
		Timeout    int         `json:"timeout_seconds"`
		Reason     string      `json:"reason"`
		Change     string      `json:"proposed_change"`
		Validation string      `json:"validation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return nil, err
	}
	record := Record{Tool: name, Arguments: args}
	switch name {
	case "read_code":
		if a.Ranges != nil {
			return s.readRanges(args, a.Path != "" || a.First != 0 || a.Count != 0, a.Ranges)
		}
		doc, err := s.readSource(a.Path, a.First, a.Count)
		if err != nil {
			return nil, err
		}
		record.Document = doc
	case "run", "install_environment":
		if s.Config.ProfilesRoot != "" && !has(s.granted, "runtime") {
			return nil, errors.New("request the runtime capability for a named hypothesis first")
		}
		if len(a.Script) > 32768 {
			return nil, errors.New("auxiliary command is too large")
		}
		seconds := a.Timeout
		if seconds == 0 {
			seconds = 120
		}
		if seconds < 1 || seconds > 600 {
			return nil, errors.New("command timeout must be 1..600 seconds")
		}
		call, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
		if err := s.owned(call); err != nil {
			return nil, err
		}
		script := a.Script
		if name == "install_environment" {
			script = "sh /input/install.sh"
		}
		if script == "" {
			return nil, errors.New("script is required")
		}
		out, err := s.docker(call, "exec", "-w", "/workspace", s.Config.Container, "sh", "-lc", script)
		if err != nil {
			out = append(out, []byte("\nCommand failed: "+err.Error())...)
		}
		id := fmt.Sprintf("tool-%d", time.Now().UnixNano())
		record.Document = &Document{Source: "replay", Prefix: id + ":", First: 1, Lines: strings.Split(strings.TrimRight(string(out), "\n"), "\n")}
	case "request_environment_change":
		if a.Reason == "" || a.Change == "" || a.Validation == "" {
			return nil, errors.New("reason, proposed_change and validation are required")
		}
		record.Request = &esa.EnvironmentRequest{Reason: a.Reason, ProposedChange: a.Change, Validation: a.Validation, State: "requested"}
	default:
		return nil, errors.New("tool is not allowed")
	}
	b, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.Config.AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(append(b, '\n'))
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	result := map[string]any{"environment_request": record.Request}
	if record.Document != nil {
		result["evidence"] = Numbered(*record.Document)
	}
	return result, nil
}

// readRange is one file range of a batched read_code.
type readRange struct {
	Path  string `json:"path"`
	First int    `json:"first_line"`
	Count int    `json:"line_count"`
}

// maxRanges bounds one batched read_code.
const maxRanges = 5

// readSource reads one range of a registered file after verifying the
// file's recorded hash.
func (s *Server) readSource(path string, first, count int) (*Document, error) {
	path = filepath.ToSlash(filepath.Clean(path))
	expected, ok := s.Config.Files[path]
	if !ok || first < 1 || count < 1 || count > 200 {
		return nil, errors.New("choose a registered source path and 1..200 lines")
	}
	root, err := os.OpenRoot(s.Config.SourceDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(f, hash))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var lines []string
	line := 0
	for scanner.Scan() {
		line++
		if line >= first && line < first+count {
			lines = append(lines, scanner.Text())
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return nil, errors.New("historical source hash changed")
	}
	if len(lines) == 0 {
		return nil, errors.New("source line range is empty")
	}
	return &Document{Source: "git", Prefix: s.Config.Commit + ":" + path + ":", First: first, Lines: lines}, nil
}

// readRanges reads up to maxRanges file ranges in one call, so the Agent
// can look at several places without a model call per read. Every range is
// read before anything is audited; each is then audited as its own
// read_code record, so evidence validation sees ordinary single reads.
func (s *Server) readRanges(args json.RawMessage, mixed bool, ranges []readRange) (map[string]any, error) {
	if mixed || len(ranges) == 0 || len(ranges) > maxRanges {
		return nil, fmt.Errorf("pass either path/first_line/line_count or ranges with 1..%d entries", maxRanges)
	}
	records := make([]Record, 0, len(ranges))
	documents := make([]NumberedDocument, 0, len(ranges))
	for i, r := range ranges {
		doc, err := s.readSource(r.Path, r.First, r.Count)
		if err != nil {
			return nil, fmt.Errorf("range %d (%s): %w", i+1, r.Path, err)
		}
		single, _ := json.Marshal(r)
		records = append(records, Record{Tool: "read_code", Arguments: single, Document: doc})
		documents = append(documents, Numbered(*doc))
	}
	f, err := os.OpenFile(s.Config.AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	var batch []byte
	for _, record := range records {
		b, err := json.Marshal(record)
		if err != nil {
			f.Close()
			return nil, err
		}
		batch = append(append(batch, b...), '\n')
	}
	_, err = f.Write(batch)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"documents": documents}, nil
}

func tools(extended bool) []map[string]any {
	str := func() map[string]string { return map[string]string{"type": "string"} }
	number := func() map[string]string { return map[string]string{"type": "integer"} }
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	result := []map[string]any{
		{
			"name": "read_code",
			"description": "Read frozen historical source. Returns a header naming the file, then one line each as " +
				"<line>#<check> | text. A line's evidence ID is <file>:<line>#<check> (the header gives an example, such as " +
				"Client.java:350#a3f9); use it exactly in finding locations and evidence_ids, never count lines manually or change any part. " +
				"The header's line range is the range actually returned, including at end of file. To read several places at once, " +
				"pass ranges (up to 5 objects with path, first_line and line_count) instead of path/first_line/line_count; each comes back " +
				"as its own block.",
			"inputSchema": schema(map[string]any{"path": str(), "first_line": number(), "line_count": number(),
				"ranges": map[string]any{"type": "array", "maxItems": maxRanges, "items": schema(map[string]any{"path": str(), "first_line": number(), "line_count": number()}, "path", "first_line", "line_count")}}),
		},
		{
			"name": "run",
			"description": "Run installation, the system or debugger commands inside the isolated replay container. " +
				"Product source and runtime inputs are read-only. Write auxiliary scripts and compiled " +
				"tools only under /workspace. No host filesystem, credentials or Docker socket is exposed. " +
				"Output is capped at 64 KiB. Output lines carry evidence IDs the same way, such as run-48213:42#9c1e.",
			"inputSchema": schema(map[string]any{"script": str(), "timeout_seconds": number()}, "script"),
		},
		{
			"name": "install_environment",
			"description": "Initialize the registered workspace and inspect its target Java toolchain. " +
				"Scenario-specific inputs are available only when explicitly registered. No product source " +
				"is changed.",
			"inputSchema": schema(map[string]any{}),
		},
		{
			"name":        "request_environment_change",
			"description": "Record an environment defect and a proposed change for engineer review. Does not apply the change or modify product source.",
			"inputSchema": schema(map[string]any{"reason": str(), "proposed_change": str(), "validation": str()}, "reason", "proposed_change", "validation"),
		},
	}
	if extended {
		result = append(result, extendedTools()...)
	}
	return result
}

// Serve uses MCP's newline-delimited stdio transport. Diagnostics never go to stdout.
// Serve handles MCP JSON-RPC over stdio and cleans owned runtime resources on exit.
// Protocol output must stay on stdout; diagnostics belong on stderr.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) (resultErr error) {
	defer func() {
		if s.cleanup != nil {
			resultErr = errors.Join(resultErr, s.cleanup())
		}
	}()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(out)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return err
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		var err error
		switch request.Method {
		case "initialize":
			var p struct {
				Version string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(request.Params, &p)
			result = map[string]any{
				"protocolVersion": p.Version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "esa_replay", "version": "0.2.0"},
			}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			available := tools(s.Config.ProfilesRoot != "")
			if s.Config.ProfilesRoot != "" && s.Config.Maven != nil {
				available = append(available, mavenTool())
			}
			if s.Config.IssuesOnly {
				available = routingTools(s.Config.IssueContext != "")
			}
			result = map[string]any{"tools": available}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			err = json.Unmarshal(request.Params, &p)
			var r map[string]any
			if err == nil {
				r, err = s.Call(ctx, p.Name, p.Arguments)
			}
			text := ""
			if err != nil {
				text = err.Error()
			} else {
				text = renderResult(p.Name, r)
			}
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": err != nil}
			err = nil
		default:
			err = errors.New("method not supported")
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}
		if err != nil {
			delete(response, "result")
			response["error"] = map[string]any{"code": -32601, "message": err.Error()}
		}
		if err = encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}
