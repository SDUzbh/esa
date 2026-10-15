package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"esa/internal/capabilities"
	"esa/internal/esa"
	"esa/internal/replay"
	"esa/internal/replaytools"
)

type Registration struct {
	Packet           Packet
	ToolsConfig      string
	SetupEnvironment func(context.Context, string) (string, func() error, error)
}
type ReleaseRegistration struct {
	ProjectID   string    `json:"project_id"`
	Version     string    `json:"version"`
	Ref         string    `json:"ref"`
	Repository  string    `json:"repository"`
	Scope       string    `json:"scope"`
	AvailableAt time.Time `json:"available_at"`
	PacketPath  string    `json:"packet_path,omitempty"`
}
type Registry struct {
	Releases          []ReleaseRegistration `json:"releases"`
	Artifacts         string                `json:"-"`
	ProfilesRoot      string                `json:"-"`
	EnvironmentScript string                `json:"-"`
	mu                sync.Mutex
}

func LoadRegistry(path, artifacts, profiles, script string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &Registry{Artifacts: artifacts, ProfilesRoot: profiles, EnvironmentScript: script}
	if err = json.Unmarshal(data, r); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, release := range r.Releases {
		key := release.ProjectID + "\x00" + release.Version
		if seen[key] || release.ProjectID == "" || release.Version == "" || release.Ref == "" || !filepath.IsAbs(release.Repository) || release.AvailableAt.IsZero() || (release.Scope != "clients" && release.Scope != "full") {
			return nil, errors.New("release registry requires unique project/version, local repository, ref, scope and availability")
		}
		seen[key] = true
	}
	return r, nil
}
func (r *Registry) Resolve(ctx context.Context, in esa.AnalyzeRequest) (Registration, error) {
	var result Registration
	if !in.ReadOnly || in.Ticket.Private {
		return result, errors.New("source resolution requires an authorized read-only ticket")
	}
	cutoff := in.Ticket.ForwardedAt
	if in.Contract != nil {
		cutoff = in.Contract.ObservationCutoff
	}
	if cutoff.IsZero() {
		return result, errors.New("source resolution requires an observation cutoff")
	}
	unavailable := func(reason string) (Registration, error) {
		return Registration{Packet: Packet{Ticket: in.Ticket, Cutoff: cutoff, Limitations: []string{reason + " No product source or runtime is registered for this task. Request the missing environment/version information; do not invent code locations or claim a reproduction."}}}, nil
	}
	if len(in.Ticket.Versions) != 1 {
		return unavailable("One unambiguous affected release is needed for source access.")
	}
	var selected *ReleaseRegistration
	for i := range r.Releases {
		release := &r.Releases[i]
		if release.ProjectID == in.Ticket.ProjectID && release.Version == in.Ticket.Versions[0] && !release.AvailableAt.After(cutoff) {
			selected = release
		}
	}
	if selected == nil {
		return unavailable("Project/release has no time-eligible source registration.")
	}
	git := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "git", append([]string{"-C", selected.Repository}, args...)...).Output()
	}
	b, err := git("rev-parse", "--verify", "--end-of-options", selected.Ref+"^{commit}")
	if err != nil {
		return result, errors.New("registered release ref is unavailable")
	}
	commit := strings.TrimSpace(string(b))
	b, err = git("show", "-s", "--format=%aI%n%cI", commit)
	if err != nil {
		return result, err
	}
	for _, ts := range strings.Fields(string(b)) {
		at, e := time.Parse(time.RFC3339, ts)
		if e != nil || at.After(cutoff) {
			return result, errors.New("registered source commit exceeds observation cutoff")
		}
	}
	// Runtime scenarios require the original diagnostic input. A generic release
	// never silently receives a Kafka-specific reproduction fixture.
	if selected.PacketPath != "" {
		data, e := os.ReadFile(selected.PacketPath)
		if e != nil {
			return result, e
		}
		var packet Packet
		if e = json.Unmarshal(data, &packet); e != nil {
			return result, e
		}
		expected := packet.Ticket
		expected.SourceKey = in.Ticket.SourceKey
		if packet.Commit == commit && !packet.Cutoff.After(cutoff) && Fingerprint(expected) == Fingerprint(in.Ticket) {
			packet.Ticket = in.Ticket
			result.Packet = packet
			if r.EnvironmentScript != "" {
				result.SetupEnvironment = ManagedEnvironment(r.EnvironmentScript, selected.PacketPath)
			}
			return result, nil
		}
	}
	key := capabilities.Digest([]byte(selected.ProjectID + "\x00" + commit + "\x00" + selected.Scope + "\x00source-v1"))
	bundle := filepath.Join(r.Artifacts, "source-"+key)
	r.mu.Lock()
	if _, err = os.Stat(filepath.Join(bundle, "manifest.json")); os.IsNotExist(err) {
		raw, _ := json.Marshal(in.Ticket)
		plan := replay.Plan{Ticket: in.Ticket, Cutoff: cutoff, Commit: commit, Ref: selected.Ref, RawHash: capabilities.Digest(raw)}
		err = replay.Export(ctx, plan, selected.Repository, bundle, selected.Scope)
	}
	r.mu.Unlock()
	if err != nil {
		return result, err
	}
	manifestData, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		return result, err
	}
	var manifest struct {
		Commit string            `json:"source_commit"`
		Files  map[string]string `json:"files"`
	}
	if err = json.Unmarshal(manifestData, &manifest); err != nil || manifest.Commit != commit || len(manifest.Files) == 0 {
		return result, errors.New("invalid cached source manifest")
	}
	result.Packet = Packet{Cutoff: cutoff, Commit: commit, Ticket: in.Ticket, Limitations: []string{"Only frozen source is registered for this release; no product runtime or causal reproduction is available. Git timestamps and operator release availability are source-selection provenance, not proof of historical system deployment."}}
	if r.EnvironmentScript != "" {
		result.Packet.Limitations = []string{"A generic tool workspace can be provisioned on request. No product build, reproduction program or prior runtime observations are supplied. Build the frozen source and write bounded diagnostic helpers in scratch. Request missing pinned dependencies or incompatible target runtimes; do not invent successful execution. Git timestamps are source-selection provenance, not proof of customer deployment."}
		result.SetupEnvironment = ManagedWorkspace(r.EnvironmentScript, bundle)
		return result, nil
	}
	result.SetupEnvironment = func(_ context.Context, dir string) (string, func() error, error) {
		config := replaytools.Config{Session: in.RunID, SourceDir: filepath.Join(bundle, "source"), Commit: commit, Files: manifest.Files, AuditPath: filepath.Join(dir, "tools.jsonl"), ProfilesRoot: r.ProfilesRoot, AllowedProfiles: []string{"general", "java-debug", "performance", "compatibility", "configuration"}, AvailableCapabilities: []string{"source"}}
		config.Readiness = map[string]any{"status": "source_only", "runtime_available": false, "source_commit": commit}
		path := filepath.Join(dir, "source-tools.json")
		data, _ := json.Marshal(config)
		err := os.WriteFile(path, data, 0600)
		return path, func() error { return nil }, err
	}
	return result, nil
}
