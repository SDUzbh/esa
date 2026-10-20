package codex

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Each prompt file is one fragment the code joins with spaces or newlines:
// a single line, no surrounding whitespace, nonempty.
func TestPromptFilesAreWellFormed(t *testing.T) {
	names, err := fs.Glob(promptFiles, "prompts/*.txt")
	if err != nil || len(names) == 0 {
		t.Fatal("no embedded prompts", err)
	}
	for _, name := range names {
		b, _ := promptFiles.ReadFile(name)
		text := strings.TrimSuffix(string(b), "\n")
		if text == "" || strings.TrimSpace(text) != text || strings.Contains(text, "\n") {
			t.Errorf("%s must be one nonempty line without surrounding whitespace", name)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(PromptVersion) {
		t.Fatal("prompt version is not a 16-hex digest:", PromptVersion)
	}
	for _, catalog := range []bool{false, true} {
		text := engineeringInstructions(true, catalog)
		if strings.Contains(text, "  ") || strings.Contains(text, "capability_request before runtime work") != catalog {
			t.Errorf("engineering instructions joined badly (catalog=%v)", catalog)
		}
	}
	// The catalog variant leaves the runtime procedure to the capability grant.
	runtime := promptText("engineering-runtime")
	if catalog := engineeringInstructions(true, true); strings.Contains(catalog, runtime) || strings.Contains(catalog, "maven_install") || strings.Contains(catalog, "/workspace/classes") {
		t.Error("the catalog instructions still carry the runtime procedure")
	}
}
