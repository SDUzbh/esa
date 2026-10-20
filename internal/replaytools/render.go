package replaytools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// renderResult is the text the model sees for a tool result. Numbered
// documents and search hits are compact text: as JSON, each line or hit
// repeated its keys, commit and file hash, and about half of the output was
// bookkeeping rather than code. Other results stay JSON. The result maps, the
// audit and evidence validation are unchanged.
func renderResult(name string, r map[string]any) string {
	if doc, ok := r["evidence"].(NumberedDocument); ok {
		text := renderDocument(doc)
		if request, _ := json.Marshal(r["environment_request"]); string(request) != "null" {
			text += "\nenvironment_request: " + string(request)
		}
		return text
	}
	if hits, ok := r["hits"].([]map[string]any); ok && name == "source_search" {
		truncated, _ := r["truncated"].(bool)
		return renderSearch(hits, truncated)
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// renderDocument prints a header naming the source once, then one line each
// as <line>#<check> | text. A line's evidence ID is <label>:<line>#<check>.
func renderDocument(d NumberedDocument) string {
	label := evidenceLabel(d.Source, d.Prefix)
	var b strings.Builder
	if d.Source == "git" {
		ref := strings.TrimSuffix(d.Prefix, ":")
		commit, path, _ := strings.Cut(ref, ":")
		fmt.Fprintf(&b, "%s at commit %s, lines %d-%d\n", path, short(commit), d.First, d.Last)
	} else {
		fmt.Fprintf(&b, "%s output %s, lines %d-%d\n", d.Source, label, d.First, d.Last)
	}
	if len(d.Lines) == 0 {
		b.WriteString("(no lines)")
		return b.String()
	}
	fmt.Fprintf(&b, "Cite a line by its evidence ID %s:<line>#<check>, for example %s\n", label, d.Lines[0].ID)
	for _, line := range d.Lines {
		token := strings.TrimPrefix(line.ID, label+":")
		if token == line.ID {
			token = line.ID // never expected; print the full ID rather than a wrong suffix
		}
		fmt.Fprintf(&b, "%s | %s\n", token, line.Text)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// renderSearch states the commit once and groups hits by file; hits are for
// discovery, so they carry no evidence IDs or file hashes.
func renderSearch(hits []map[string]any, truncated bool) string {
	var b strings.Builder
	commit := ""
	if len(hits) > 0 {
		commit, _ = hits[0]["commit"].(string)
	}
	noun := "hits"
	if len(hits) == 1 {
		noun = "hit"
	}
	fmt.Fprintf(&b, "%d literal %s at commit %s", len(hits), noun, short(commit))
	if truncated {
		b.WriteString(" (limit reached; more exist, narrow the query)")
	}
	b.WriteString(". Read a hit with read_code to cite it.")
	file := ""
	for _, hit := range hits {
		if f, _ := hit["file"].(string); f != file {
			file = f
			b.WriteString("\n" + file)
		}
		text, _ := hit["text"].(string)
		fmt.Fprintf(&b, "\n  %v: %s", hit["line"], strings.TrimSpace(text))
	}
	return b.String()
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
