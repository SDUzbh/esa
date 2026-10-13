package replaytools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ManagedEnvironment starts an operator-owned environment helper, not a model
// command. Closing stdin requests normal cleanup before a process is terminated.
func ManagedEnvironment(script, evidence, profile string) func(context.Context, string) (string, func() error, error) {
	return func(ctx context.Context, dir string) (string, func() error, error) {
		cmd := exec.Command("python3", script, "--evidence", evidence, "--run-dir", dir, "--profile", profile)
		for _, value := range os.Environ() {
			if len(value) < 4 || value[:4] != "ESA_" {
				cmd.Env = append(cmd.Env, value)
			}
		}
		stderr, err := os.OpenFile(filepath.Join(dir, "environment-helper.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			return "", nil, err
		}
		cmd.Stderr = stderr
		input, err := cmd.StdinPipe()
		if err != nil {
			stderr.Close()
			return "", nil, err
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			stderr.Close()
			return "", nil, err
		}
		if err = cmd.Start(); err != nil {
			stderr.Close()
			return "", nil, err
		}
		finished := make(chan error, 1)
		done := make(chan struct{})
		monitorExited := make(chan struct{})
		go func() {
			defer close(monitorExited)
			select {
			case <-ctx.Done():
				_ = cmd.Process.Signal(syscall.SIGTERM)
			case <-done:
			}
		}()
		// Wait only after the ready line has been consumed; stdout remains open until cleanup.
		cleanup := func() error {
			_ = input.Close()
			go func() { finished <- cmd.Wait() }()
			defer stderr.Close()
			select {
			case err := <-finished:
				return err
			case <-time.After(30 * time.Second):
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-finished:
				return err
			case <-time.After(10 * time.Second):
			}
			_ = cmd.Process.Kill()
			<-finished
			return fmt.Errorf("environment helper did not finish cleanup; use replay_pipeline.py --cleanup")
		}
		reader := bufio.NewReader(output)
		line, err := reader.ReadBytes('\n')
		close(done)
		<-monitorExited
		if err != nil {
			_ = cleanup()
			return "", nil, fmt.Errorf("environment setup failed; inspect private helper log")
		}
		var ready struct {
			Config string `json:"tools_config"`
		}
		if err = json.Unmarshal(line, &ready); err != nil || ready.Config == "" {
			_ = cleanup()
			return "", nil, fmt.Errorf("invalid environment readiness response")
		}
		return ready.Config, cleanup, nil
	}
}
