package systemreplay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func pointer(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	for _, part := range strings.Split(path[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[part]
			if !ok {
				return nil, false
			}
		case []any:
			i, e := strconv.Atoi(part)
			if e != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}
func lookup(vars map[string]any, key string) (any, bool) {
	return pointer(vars, "/"+strings.ReplaceAll(key, ".", "/"))
}
func render(value any, vars map[string]any) (any, error) {
	switch x := value.(type) {
	case string:
		if strings.HasPrefix(x, "{{") && strings.HasSuffix(x, "}}") && strings.Count(x, "{{") == 1 {
			if v, ok := lookup(vars, x[2:len(x)-2]); ok {
				return clone(v), nil
			}
			return nil, fmt.Errorf("missing template variable: %s", x)
		}
		var rendered strings.Builder
		for strings.Contains(x, "{{") {
			start := strings.Index(x, "{{")
			end := strings.Index(x[start+2:], "}}")
			if end < 0 {
				return nil, fmt.Errorf("invalid template")
			}
			end += start + 2
			v, ok := lookup(vars, x[start+2:end])
			if !ok {
				return nil, fmt.Errorf("missing template variable")
			}
			rendered.WriteString(x[:start])
			rendered.WriteString(fmt.Sprint(v))
			x = x[end+2:]
		}
		rendered.WriteString(x)
		return rendered.String(), nil
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			r, e := render(v, vars)
			if e != nil {
				return nil, e
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := []any{}
		for _, v := range x {
			r, e := render(v, vars)
			if e != nil {
				return nil, e
			}
			out = append(out, r)
		}
		return out, nil
	default:
		return value, nil
	}
}
func renderString(s string, vars map[string]any) (string, error) {
	v, e := render(s, vars)
	if e != nil {
		return "", e
	}
	return fmt.Sprint(v), nil
}
func assertions(value any, as []Assertion) ([]Check, error) {
	out := []Check{}
	for _, a := range as {
		actual, exists := pointer(value, a.Path)
		passed := false
		switch a.Op {
		case "eq":
			passed = exists && equal(actual, a.Value)
		case "ne":
			passed = exists && !equal(actual, a.Value)
		case "exists":
			passed = exists
		case "absent":
			passed = !exists
		case "contains":
			passed = exists && strings.Contains(fmt.Sprint(actual), fmt.Sprint(a.Value))
		case "not_contains":
			passed = exists && !strings.Contains(fmt.Sprint(actual), fmt.Sprint(a.Value))
		default:
			return out, fmt.Errorf("unknown assertion operator: %s", a.Op)
		}
		out = append(out, Check{a, passed, actual})
	}
	return out, nil
}
func passed(cs []Check) bool {
	if len(cs) == 0 {
		return false
	}
	for _, c := range cs {
		if !c.Passed {
			return false
		}
	}
	return true
}
func first(v any, paths []string) any {
	for _, p := range paths {
		if value, ok := pointer(v, p); ok && value != nil && value != "" {
			return value
		}
	}
	return nil
}
func extract(body any, e Extract) (Observation, error) {
	out := Observation{RunID: text(first(body, e.RunID)), State: text(first(body, e.State)), Category: text(first(body, e.Category)), Route: text(first(body, e.Route)), IssueID: text(first(body, e.IssueID)), Raw: body}
	if value := first(body, e.Findings); value != nil {
		b, err := json.Marshal(value)
		if err != nil {
			return out, err
		}
		if err = json.Unmarshal(b, &out.Findings); err != nil {
			return out, fmt.Errorf("invalid public findings: %w", err)
		}
	}
	out.Usage, _ = first(body, e.Usage).(map[string]any)
	return out, nil
}

type Process struct {
	cmd  *exec.Cmd
	done chan error
	log  *os.File
}

func command(c Command, vars map[string]any, base string) (*exec.Cmd, error) {
	if len(c.Args) == 0 {
		return nil, fmt.Errorf("explicit command required")
	}
	args := []string{}
	for _, a := range c.Args {
		s, e := renderString(a, vars)
		if e != nil {
			return nil, e
		}
		args = append(args, s)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append([]string{}, os.Environ()...)
	for k, v := range c.Env {
		s, e := renderString(v, vars)
		if e != nil {
			return nil, e
		}
		cmd.Env = append(cmd.Env, k+"="+s)
	}
	dir := c.Dir
	if dir == "" {
		dir = base
	}
	var e error
	dir, e = renderString(dir, vars)
	if e != nil {
		return nil, e
	}
	cmd.Dir = absolute(base, dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}
func start(c Command, vars map[string]any, base, logPath string) (*Process, error) {
	cmd, e := command(c, vars, base)
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if e = cmd.Start(); e != nil {
		_ = f.Close()
		return nil, e
	}
	p := &Process{cmd, make(chan error, 1), f}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}
func (p *Process) close() error {
	if p == nil {
		return nil
	}
	defer p.log.Close()
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		return nil
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-p.done:
			return nil
		case <-time.After(5 * time.Second):
			return fmt.Errorf("owned target process did not stop")
		}
	}
}
func runCommand(ctx context.Context, c Command, vars map[string]any, base, logPath string) error {
	p, e := start(c, vars, base, logPath)
	if e != nil {
		return e
	}
	seconds := c.TimeoutSeconds
	if seconds == 0 {
		seconds = 60
	}
	select {
	case e := <-p.done:
		_ = p.log.Close()
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		return e
	case <-ctx.Done():
		_ = p.close()
		return ctx.Err()
	case <-time.After(time.Duration(seconds) * time.Second):
		_ = p.close()
		return fmt.Errorf("command timed out")
	}
}

type Client struct {
	target Target
	token  string
	http   *http.Client
}

func newClient(t Target, token string) (*Client, error) {
	u, e := url.Parse(t.URL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("valid target HTTP URL without credentials required")
	}
	return &Client{t, token, &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("redirects are not allowed for authenticated replay requests")
	}}}, nil
}
func (c *Client) request(ctx context.Context, spec Request, vars map[string]any) (HTTPRecord, error) {
	var rec HTTPRecord
	method := spec.Method
	if method == "" {
		method = "GET"
	}
	path, e := renderString(spec.Path, vars)
	if e != nil {
		return rec, e
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return rec, fmt.Errorf("request path must stay on configured target")
	}
	body, e := render(spec.Body, vars)
	if e != nil {
		return rec, e
	}
	b, e := json.Marshal(body)
	if e != nil {
		return rec, e
	}
	var reader io.Reader
	if spec.Body != nil {
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.target.URL, "/")+path, reader)
	if e != nil {
		return rec, e
	}
	req.Header.Set("Content-Type", "application/json")
	for _, headers := range []map[string]string{c.target.Headers, spec.Headers} {
		for k, v := range headers {
			s, e := renderString(v, vars)
			if e != nil {
				return rec, e
			}
			req.Header.Set(k, s)
		}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, e := c.http.Do(req)
	if e != nil {
		return rec, e
	}
	defer response.Body.Close()
	const maxResponse = 12 << 20
	data, e := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if e != nil {
		return rec, e
	}
	if len(data) > maxResponse {
		return rec, fmt.Errorf("target response exceeds %d bytes", maxResponse)
	}
	if c.token != "" {
		data = bytes.ReplaceAll(data, []byte(c.token), []byte("[REDACTED]"))
	}
	rec = HTTPRecord{Method: method, Path: path, Status: response.StatusCode}
	if e = json.Unmarshal(data, &rec.Body); e != nil {
		return rec, fmt.Errorf("target response is not JSON (HTTP %d)", rec.Status)
	}
	return rec, nil
}
func (c *Client) poll(ctx context.Context, spec Request, vars map[string]any) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.target.TimeoutSeconds)*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Duration(c.target.PollMilliseconds) * time.Millisecond)
	defer ticker.Stop()
	for {
		rec, e := c.request(ctx, spec, vars)
		if e != nil {
			return Observation{}, e
		}
		if rec.Status >= 300 {
			return Observation{}, fmt.Errorf("observe returned HTTP %d", rec.Status)
		}
		out, e := extract(rec.Body, c.target.Extract)
		if e != nil {
			return out, e
		}
		for _, terminal := range c.target.TerminalStates {
			if out.State == terminal {
				return out, nil
			}
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ticker.C:
		}
	}
}
func health(ctx context.Context, c *Client) bool {
	r, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.target.URL, "/")+c.target.HealthPath, nil)
	if e != nil {
		return false
	}
	response, e := c.http.Do(r)
	if e != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == 200
}
func waitHealth(ctx context.Context, c *Client, p *Process) error {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if health(ctx, c) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-p.done:
			p.done <- e
			return fmt.Errorf("target exited before ready: %v", e)
		case <-timer.C:
			return fmt.Errorf("target health timeout")
		case <-ticker.C:
		}
	}
}
func randomID() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func redactLog(path, token string) {
	if token == "" {
		return
	}
	b, e := os.ReadFile(path)
	if e == nil {
		_ = os.WriteFile(path, bytes.ReplaceAll(b, []byte(token), []byte("[REDACTED]")), 0600)
	}
}
func varsFor(p Plan, root string) map[string]any {
	return map[string]any{"ticket": clone(p.Ticket), "ticket_id": p.Ticket["id"], "source_key": p.Ticket["source_key"], "project_id": p.Ticket["project_id"], "default_project": p.Ticket["project_id"], "event_time": p.At, "commit": p.Commit, "run_dir": root, "manifest_dir": ""}
}
func observationValue(o Observation) any {
	b, _ := json.Marshal(o)
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}
