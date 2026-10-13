package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"esa/internal/replaytools"
)

// Register frozen source immediately; provision a runtime only after an approved
// Profile request. The provider retains an owned-resource cleanup fallback.
func ManagedEnvironment(script, evidence string) func(context.Context, string) (string, func() error, error) {
	return func(ctx context.Context, dir string) (string, func() error, error) {
		scriptPath, err := filepath.Abs(script)
		if err != nil {
			return "", nil, err
		}
		evidencePath, err := filepath.Abs(evidence)
		if err != nil {
			return "", nil, err
		}
		data, err := os.ReadFile(evidencePath)
		if err != nil {
			return "", nil, err
		}
		var packet Packet
		if err = json.Unmarshal(data, &packet); err != nil {
			return "", nil, err
		}
		bundle := filepath.Dir(evidencePath)
		data, err = os.ReadFile(filepath.Join(bundle, "manifest.json"))
		if err != nil {
			return "", nil, err
		}
		var manifest struct {
			Commit string            `json:"source_commit"`
			Files  map[string]string `json:"files"`
		}
		if err = json.Unmarshal(data, &manifest); err != nil || manifest.Commit != packet.Commit {
			return "", nil, fmt.Errorf("registered packet/source mismatch")
		}
		runtimeDir := filepath.Join(dir, "runtime")
		config := replaytools.Config{Session: filepath.Base(dir), SourceDir: filepath.Join(bundle, "source"), Commit: packet.Commit, Files: manifest.Files, AuditPath: filepath.Join(dir, "tools.jsonl"), AvailableCapabilities: []string{"source", "runtime", "debugger", "temporary_tools"}, Runtime: &replaytools.RuntimeSetup{Script: scriptPath, Evidence: evidencePath, Directory: runtimeDir}, Readiness: map[string]any{"status": "source_ready", "runtime_available": false}}
		path := filepath.Join(dir, "tools-config.json")
		data, err = json.Marshal(config)
		if err == nil {
			err = os.WriteFile(path, data, 0600)
		}
		if err != nil {
			return "", nil, err
		}
		cleanup := func() error {
			cmd := exec.Command("python3", scriptPath, "--cleanup-owned", "--run-dir", runtimeDir)
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("owned environment cleanup: %w: %s", err, output)
			}
			return nil
		}
		return path, cleanup, nil
	}
}
