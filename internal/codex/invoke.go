package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"esa/internal/esa"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

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
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
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
			continue
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
	if scanner.Err() != nil {
		return nil, usage, alerts, scanner.Err()
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
