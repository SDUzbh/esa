package codex

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
)

// CheckCLIFeatures asks the installed CLI which features it knows and their
// effective state under the feature overrides that CLIArgs passes on every
// invocation. Tool enforcement during a run only cancels after a disallowed
// event, so prevention rests on these flags. A CLI release that renamed or
// dropped one would silently leave that feature at its default, so ESA refuses
// to start unless every override is known and in effect. Features named in
// accepted, an explicit operator decision, are reported as warnings instead.
func CheckCLIFeatures(ctx context.Context, cli string, accepted []string) ([]string, error) {
	want := map[string]string{}
	args := []string{"features", "list"}
	base := CLIArgs("", "", "")
	for i := 0; i+1 < len(base); i++ {
		if base[i] == "-c" && strings.HasPrefix(base[i+1], "features.") {
			name, value, _ := strings.Cut(strings.TrimPrefix(base[i+1], "features."), "=")
			want[name] = value
			args = append(args, "-c", base[i+1])
		}
	}
	out, err := exec.CommandContext(ctx, cli, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("codex features list failed: %w", err)
	}
	// Each line is: name, stage (possibly several words), effective state.
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			got[fields[0]] = fields[len(fields)-1]
		}
	}
	var problems, warnings []string
	for _, name := range slices.Sorted(maps.Keys(want)) {
		problem := ""
		switch state, known := got[name]; {
		case !known:
			problem = name + " is unknown"
		case state != want[name]:
			problem = name + " is " + state + ", want " + want[name]
		}
		switch {
		case problem == "":
		case slices.Contains(accepted, name):
			warnings = append(warnings, problem)
		default:
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		version, _ := exec.CommandContext(ctx, cli, "--version").Output()
		return warnings, fmt.Errorf("the Codex CLI (%s) does not honour ESA's feature settings: %s; use a CLI release that does, update CLIArgs, or accept the risk with -accept-cli-feature",
			strings.TrimSpace(string(version)), strings.Join(problems, "; "))
	}
	return warnings, nil
}
