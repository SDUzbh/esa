package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"esa/internal/replaytools"
)

func TestManagedEnvironmentRegistersSourceWithoutLaunchingHelper(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	packet := filepath.Join(dir, "evidence.json")
	if err := os.WriteFile(packet, []byte(`{"source_commit":"pinned"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"source_commit":"pinned","files":{"Example.java":"hash"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// A nonexistent helper proves setup did not execute it for source inspection.
	path, cleanup, err := ManagedEnvironment(filepath.Join(dir, "not-started.py"), packet)(context.Background(), dir)
	if err != nil || cleanup == nil {
		t.Fatal(path, err)
	}
	config, err := replaytools.Load(path)
	if err != nil || config.Container != "" || config.Runtime == nil || config.Commit != "pinned" {
		t.Fatal("invalid deferred registration", config, err)
	}
	if _, err = os.Stat(config.Runtime.Directory); !os.IsNotExist(err) {
		t.Fatal("source registration provisioned a runtime", err)
	}
}
