package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFeatureCLI writes a CLI whose "features list" prints listing and whose
// --version prints a fixed release.
func fakeFeatureCLI(t *testing.T, listing string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "listing"), []byte(listing), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = --version ] && { echo codex-cli 9.9.9; exit 0; }\n[ \"$1 $2\" = 'features list' ] || exit 3\ncat " + filepath.Join(dir, "listing") + "\n"
	cli := filepath.Join(dir, "codex")
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return cli
}

// honoured lists every feature CLIArgs sets, in the effective state it sets.
func honoured() string {
	var b strings.Builder
	args := CLIArgs("", "", "")
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.HasPrefix(args[i+1], "features.") {
			name, value, _ := strings.Cut(strings.TrimPrefix(args[i+1], "features."), "=")
			b.WriteString(name + "   under development   " + value + "\n")
		}
	}
	return b.String() + "fast_mode   stable   true\n"
}

func TestCLIFeatureCheckRequiresEveryOverride(t *testing.T) {
	ctx := context.Background()
	if warnings, err := CheckCLIFeatures(ctx, fakeFeatureCLI(t, honoured()), nil); err != nil || len(warnings) != 0 {
		t.Fatal("a CLI that honours every override was refused:", warnings, err)
	}
	ignored := fakeFeatureCLI(t, strings.Replace(honoured(), "shell_tool   under development   false", "shell_tool   stable   true", 1))
	if _, err := CheckCLIFeatures(ctx, ignored, nil); err == nil || !strings.Contains(err.Error(), "shell_tool is true, want false") || !strings.Contains(err.Error(), "codex-cli 9.9.9") {
		t.Fatal("an override the CLI ignores was accepted:", err)
	}
	// An operator may accept that one named feature differs; nothing else.
	if warnings, err := CheckCLIFeatures(ctx, ignored, []string{"shell_tool"}); err != nil || len(warnings) != 1 || warnings[0] != "shell_tool is true, want false" {
		t.Fatal("an explicitly accepted feature was not reported as a warning:", warnings, err)
	}
	if _, err := CheckCLIFeatures(ctx, ignored, []string{"hooks"}); err == nil {
		t.Fatal("accepting one feature excused another")
	}
	var dropped []string
	for _, line := range strings.Split(honoured(), "\n") {
		if !strings.HasPrefix(line, "hooks ") {
			dropped = append(dropped, line)
		}
	}
	if _, err := CheckCLIFeatures(ctx, fakeFeatureCLI(t, strings.Join(dropped, "\n")), nil); err == nil || !strings.Contains(err.Error(), "hooks is unknown") {
		t.Fatal("a feature the CLI no longer knows was accepted:", err)
	}
	if _, err := CheckCLIFeatures(ctx, filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("a missing CLI passed the check")
	}
}
