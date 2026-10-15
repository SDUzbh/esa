package systemreplay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Runtime serializes JSON-line calls to the separately owned product environment driver.
type Runtime struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	done    chan error
	log     *os.File
}

func runtimeStart(c Command, vars map[string]any, base, root string) (*Runtime, error) {
	cmd, e := command(c, vars, base)
	if e != nil {
		return nil, e
	}
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(root, "runtime-driver.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	cmd.Stderr = f
	if e = cmd.Start(); e != nil {
		_ = f.Close()
		return nil, e
	}
	r := &Runtime{cmd: cmd, stdin: in, scanner: bufio.NewScanner(out), done: make(chan error, 1), log: f}
	r.scanner.Buffer(make([]byte, 4096), 16<<20)
	go func() { r.done <- cmd.Wait() }()
	return r, nil
}
func (r *Runtime) call(ctx context.Context, request any) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(request)
	if _, e := r.stdin.Write(append(b, '\n')); e != nil {
		return nil, e
	}
	type reply struct {
		value map[string]any
		err   error
	}
	ch := make(chan reply, 1)
	go func() {
		if !r.scanner.Scan() {
			ch <- reply{err: fmt.Errorf("runtime driver exited: %v", r.scanner.Err())}
			return
		}
		var v struct {
			OK     bool           `json:"ok"`
			Result map[string]any `json:"result"`
			Error  string         `json:"error"`
		}
		if e := json.Unmarshal(r.scanner.Bytes(), &v); e != nil {
			ch <- reply{err: e}
			return
		}
		if !v.OK {
			ch <- reply{err: fmt.Errorf("runtime: %s", v.Error)}
			return
		}
		ch <- reply{value: v.Result}
	}()
	select {
	case v := <-ch:
		return v.value, v.err
	case <-ctx.Done():
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-ch:
			case <-time.After(5 * time.Second):
			}
		}
		return nil, ctx.Err()
	}
}
func (r *Runtime) close() error {
	if r == nil {
		return nil
	}
	defer r.log.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, e := r.call(ctx, map[string]string{"action": "close"})
	_ = r.stdin.Close()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		<-r.done
		if e == nil {
			e = fmt.Errorf("runtime driver required forced termination")
		}
	}
	return e
}
func observeRuntime(ctx context.Context, r *Runtime, m Manifest, p Plan, commit, root string) (out RuntimeRecord, err error) {
	out = RuntimeRecord{Commit: commit}
	if p.Job.Scenario == nil {
		return out, fmt.Errorf("no registered replay scenario")
	}
	scenario := *p.Job.Scenario
	b, _ := json.Marshal(scenario)
	out.ScenarioHash = sha(b)
	source, files, e := export(ctx, m.Repo, commit, root)
	if e != nil {
		return out, e
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	defer func() {
		reset, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		response, e := r.call(reset, map[string]string{"action": "reset"})
		if e == nil && response["reset"] != true {
			e = fmt.Errorf("runtime reset was not acknowledged")
		}
		if e != nil {
			err = errors.Join(err, fmt.Errorf("runtime reset: %w", e))
		}
	}()
	out.Setup, e = r.call(call, map[string]any{"action": "prepare", "commit": commit, "source_dir": source, "files": files, "profile": scenario.Profile})
	if e != nil {
		return out, e
	}
	if ready, _ := out.Setup["ready"].(bool); !ready {
		return out, fmt.Errorf("runtime setup is not ready")
	}
	out.Outcome, e = r.call(call, map[string]any{"action": "observe", "command": scenario.Command, "timeout_seconds": 180})
	if e != nil {
		return out, e
	}
	out.Controls, e = assertions(out.Outcome, scenario.Controls)
	if e != nil {
		return out, e
	}
	out.Bug, e = assertions(out.Outcome, scenario.Bug)
	if e != nil {
		return out, e
	}
	out.Healthy, e = assertions(out.Outcome, scenario.Healthy)
	if e != nil {
		return out, e
	}
	return out, nil
}
func repairState(baseline, before, after RuntimeRecord) string {
	if baseline.ScenarioHash == "" || baseline.ScenarioHash != before.ScenarioHash || before.ScenarioHash != after.ScenarioHash {
		return "scenario_changed"
	}
	if !passed(baseline.Controls) || !passed(before.Controls) || !passed(after.Controls) {
		return "environment_failure"
	}
	if !passed(baseline.Bug) {
		return "baseline_not_reproduced"
	}
	if !passed(before.Bug) {
		return "parent_not_reproduced"
	}
	if passed(after.Healthy) && !passed(after.Bug) {
		return "behavior_verified"
	}
	return "not_fixed"
}
