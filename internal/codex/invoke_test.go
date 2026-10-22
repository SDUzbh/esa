package codex

import (
	"bufio"
	"context"
	"errors"
	"io"
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
		// Long enough for the child to start even when the suite is busy.
		_, _, _, err := p.invoke(context.Background(), t.TempDir(), "prompt", "K-1", esa.Budget{TimeoutSeconds: 5})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "timeout=true") {
			t.Fatal("the timed-out call did not report its timeout", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("invoke was still blocked 20 s after a 5 s timeout")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
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

func TestReadEventBoundsLongLines(t *testing.T) {
	long := strings.Repeat("x", maxEventLine+1)
	exact := strings.Repeat("y", maxEventLine-1)
	r := bufio.NewReaderSize(strings.NewReader("short\n"+long+"\n"+exact+"\nlast"), 4096)
	for _, want := range []struct {
		line string
		size int
		eof  bool
	}{{"short", 6, false}, {"", maxEventLine + 2, false}, {exact, maxEventLine, false}, {"last", 4, true}} {
		line, size, err := readEvent(r)
		if string(line) != want.line || size != want.size || errors.Is(err, io.EOF) != want.eof {
			t.Fatalf("got %d bytes, size %d, err %v; want %d bytes, size %d", len(line), size, err, len(want.line), want.size)
		}
	}
}

// An event line over the limit stops the run at once instead of stalling the
// stage until its timeout, and the event is recorded by size, not content.
func TestInvokeStopsOnAnOversizedEvent(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "fake-codex")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		"python3 -c 'import sys; sys.stdout.write(\"{\\\"type\\\":\\\"item.completed\\\",\\\"text\\\":\\\"\" + \"x\"*3000000 + \"\\\"}\\n\"); sys.stdout.flush()'\n" +
		"while :; do echo '{\"type\":\"item.started\",\"item\":{\"type\":\"reasoning\"}}'; sleep 0.01; done\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runDir := t.TempDir()
	start := time.Now()
	_, _, _, err := (&Provider{CLI: cli}).invoke(context.Background(), runDir, "prompt", "K-1", esa.Budget{TimeoutSeconds: 60})
	if err == nil || !strings.Contains(err.Error(), "-byte event, over the") {
		t.Fatal("an oversized event did not stop the run with its own error", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatal("the run stalled for", elapsed)
	}
	events, _ := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if !strings.Contains(string(events), `"type":"esa.oversized_event"`) || len(events) > 1<<20 {
		t.Fatal("the oversized event was not recorded by size alone", len(events))
	}
}
