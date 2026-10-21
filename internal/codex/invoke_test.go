package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"esa/internal/esa"
)

// A timed-out CLI whose child keeps its pipes open must not block the call:
// the whole process group is killed, so Wait returns and no child survives.
func TestInvokeKillsTheCLIProcessGroupOnTimeout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cli := filepath.Join(dir, "fake-codex")
	script := "#!/bin/sh\ncat > /dev/null\nsleep 300 &\necho $! > '" + pidFile + "'\nsleep 300\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := &Provider{CLI: cli}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, _, err := p.invoke(context.Background(), t.TempDir(), "prompt", "K-1", esa.Budget{TimeoutSeconds: 1})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timeout=true") {
			t.Fatal("the timed-out call did not report its timeout", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("invoke was still blocked 15 s after a 1 s timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatal("invoke took", elapsed)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the CLI's child survived the timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
