package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"esa/internal/esa"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// maxEventLine bounds one CLI event line. A longer line is discarded as it
// is read, never buffered whole.
const maxEventLine = 2 << 20

// readEvent reads one newline-terminated event. It returns the line without
// its newline and the line's full size; a line over maxEventLine comes back
// empty, with its size, after the rest of it has been discarded.
func readEvent(r *bufio.Reader) ([]byte, int, error) {
	var line []byte
	size := 0
	for {
		chunk, err := r.ReadSlice('\n')
		size += len(chunk)
		if size <= maxEventLine {
			line = append(line, chunk...)
		} else {
			line = nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return bytes.TrimSuffix(line, []byte{'\n'}), size, err
		}
	}
}

// invoke records every CLI attempt, including rejected output and missing usage.
// Token thresholds warn but do not cancel the process.
func (p *Provider) invoke(ctx context.Context, dir, prompt, sourceKey string, budget esa.Budget) ([]byte, esa.Usage, []esa.Alert, error) {
	usage := esa.Usage{Calls: 1}
	var alerts []esa.Alert
	if budget.TimeoutSeconds < 1 {
		return nil, usage, alerts, errors.New("invalid timeout")
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(budget.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, p.CLI, p.cliArgs(dir)...)
	// The CLI starts children (the ESA tool server, which runs docker). Run it
	// in its own process group and kill the whole group on timeout or
	// cancellation: a surviving child would keep the CLI's stderr open, and
	// Wait would then never return, holding this engineering slot.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stdin = strings.NewReader(prompt)
	// The credential-bearing ESA service token is not passed to the CLI.
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "ESA_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, usage, alerts, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return nil, usage, alerts, err
	}
	defer log.Close()
	if err = cmd.Start(); err != nil {
		return nil, usage, alerts, err
	}
	emitAlerts := func(current esa.Usage) {
		for _, alert := range esa.TokenAlerts(current, budget) {
			seen := false
			for _, old := range alerts {
				if old.Code == alert.Code {
					seen = true
					break
				}
			}
			if seen {
				continue
			}
			alerts = append(alerts, alert)
			slog.Warn(alert.Message, "run_id", filepath.Base(dir), "source_key", sourceKey, "code", alert.Code)
			b, _ := json.Marshal(alert)
			f, e := os.OpenFile(filepath.Join(dir, "alerts.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e == nil {
				_, _ = f.Write(append(b, '\n'))
				_ = f.Close()
			}
		}
	}
	turns := 0
	unexpected := false
	quota := ""
	var lastUsage json.RawMessage
	// handle logs one event and applies the tool policy to it.
	handle := func(line []byte) {
		_, _ = log.Write(append(append([]byte{}, line...), '\n'))
		var ev struct {
			Type    string                   `json:"type"`
			Message string                   `json:"message"`
			Error   struct{ Message string } `json:"error"`
			Usage   json.RawMessage          `json:"usage"`
			Item    struct {
				Type   string `json:"type"`
				Server string `json:"server"`
				Tool   string `json:"tool"`
			} `json:"item"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if message := ev.Message + ev.Error.Message; (ev.Type == "error" || ev.Type == "turn.failed") && usageLimited(message) {
			quota = message
		}
		if ev.Type == "turn.completed" {
			turns++
			lastUsage = append(json.RawMessage{}, ev.Usage...)
			if reported, ok := reportedTokens(ev.Usage); ok {
				usage.InputTokens += reported.InputTokens
				usage.CachedInputTokens += reported.CachedInputTokens
				usage.OutputTokens += reported.OutputTokens
				usage.TokensReported = true
				emitAlerts(usage)
			}
		}
		// CLI startup warnings use an error item too. They do not execute a tool;
		// failures still cannot yield a report without one completed turn.
		permitted := allowedItem(ev.Item.Type, ev.Item.Server, ev.Item.Tool, p.ToolsConfig != "")
		if permitted && ev.Item.Type == "mcp_tool_call" && p.ToolNames != nil {
			permitted = false
			for _, name := range p.ToolNames {
				permitted = permitted || ev.Item.Tool == name
			}
		}
		if !permitted {
			unexpected = true
			cancel()
		}
	}
	reader := bufio.NewReaderSize(stdout, 64<<10)
	var readErr error
	oversized := 0
	for {
		line, size, err := readEvent(reader)
		if size > maxEventLine {
			// The event was discarded unread, so the tool policy cannot check
			// it: stop the run instead of reading past it. The process group
			// is killed, so the rest of the output drains to end of file.
			if oversized == 0 {
				oversized = size
				note, _ := json.Marshal(map[string]any{"type": "esa.oversized_event", "bytes": size, "limit": maxEventLine})
				_, _ = log.Write(append(note, '\n'))
				cancel()
			}
		} else if len(line) > 0 {
			handle(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	waitErr := cmd.Wait()
	_ = os.WriteFile(filepath.Join(dir, "stderr.log"), stderr.Bytes(), 0600)
	if !usage.TokensReported {
		emitAlerts(usage)
	}
	audit, _ := json.MarshalIndent(
		map[string]any{
			"provider":                "codex_cli",
			"prompt_version":          PromptVersion,
			"model":                   p.Model,
			"source_key":              sourceKey,
			"provider_invocations":    1,
			"completed_turns":         turns,
			"unexpected_tool_event":   unexpected,
			"cli_usage":               lastUsage,
			"usage":                   usage,
			"registered_replay_tools": p.ToolsConfig != "",
			"sandbox":                 "read-only",
			"alerts":                  alerts,
			"token_policy": "Advisory warnings only; never cancel or reject because of token consumption. Invocation " +
				"limits and explicit timeouts are separate.",
		},
		"",
		"  ",
	)
	_ = os.WriteFile(filepath.Join(dir, "audit.json"), audit, 0600)
	if readErr != nil {
		return nil, usage, alerts, readErr
	}
	if oversized > 0 {
		return nil, usage, alerts, fmt.Errorf("Codex emitted a %d-byte event, over the %d-byte limit; the run was stopped", oversized, maxEventLine)
	}
	if quota != "" && turns == 0 {
		return nil, usage, alerts, &esa.ProviderQuotaError{RetryAt: quotaReset(quota, time.Now()), Message: quota}
	}
	if waitErr != nil {
		return nil, usage, alerts, fmt.Errorf("Codex failed; inspect private run logs (timeout=%t)", callCtx.Err() != nil)
	}
	if turns != 1 || unexpected {
		return nil, usage, alerts, errors.New("Codex must complete one turn with only registered tools")
	}
	b, err := os.ReadFile(filepath.Join(dir, "answer.json"))
	if err != nil {
		return nil, usage, alerts, err
	}
	return b, usage, alerts, nil
}
