package codex

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"slices"
	"strings"
)

// The Agent instructions and prompt prefixes live in prompts/*.txt, so they
// are reviewed and versioned as text. Each file holds one fragment; the code
// that assembles them adds the joining spaces and newlines.
//
//go:embed prompts/*.txt
var promptFiles embed.FS

// promptText returns an embedded prompt file without its final newline. A
// missing file panics at start-up, never mid-run.
func promptText(name string) string {
	b, err := promptFiles.ReadFile("prompts/" + name + ".txt")
	if err != nil {
		panic(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

var (
	routingInstructions    = promptText("routing")
	routingPrompt          = promptText("routing-prompt")
	assignmentInstructions = promptText("assignment")
	assignmentPrompt       = promptText("assignment-prompt")
	reviewInstructions     = promptText("review")
	reviewPrompt           = promptText("review-prompt")
	engineeringPrompt      = promptText("engineering-prompt")
)

// PromptVersion identifies the exact prompt texts: a digest over every file's
// name and bytes. The invocation audit records it, so a run can be traced to
// the prompts it used.
var PromptVersion = func() string {
	names, err := fs.Glob(promptFiles, "prompts/*.txt")
	if err != nil {
		panic(err)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, name := range names {
		b, _ := promptFiles.ReadFile(name)
		h.Write([]byte(name + "\x00"))
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}()

// engineeringInstructions assembles the Engineering Agent's instructions:
// packet-only diagnosis or registered tools, with the catalog variant when
// approved Profiles are available, and the bootstrap Skill text.
func engineeringInstructions(tools, catalog bool, bootstrap string) string {
	base := promptText("engineering-packet")
	if tools {
		base = promptText("engineering-tools")
		if catalog {
			base = promptText("engineering-tools-catalog")
		}
	}
	text := base + " " + promptText("engineering-forwarded")
	if catalog {
		text += " " + promptText("engineering-catalog") + "\n" + bootstrap
	}
	text += " " + promptText("engineering-task") + " " + promptText("engineering-evidence")
	if catalog {
		text += " " + promptText("engineering-workspace")
	}
	return text
}
