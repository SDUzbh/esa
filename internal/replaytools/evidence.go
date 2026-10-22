package replaytools

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strconv"
	"strings"
)

// EvidenceID binds source, reference prefix, absolute line number and exact text.
// It reads as the location plus a checksum, such as `Client.java:350#a3f9` or
// `run-48213:42#9c1e`, so the Agent rebuilds it from what it cites instead of
// recalling a random string; a wrong name, line or checksum matches nothing.
// Consumers must reject ambiguous IDs rather than resolving a collision by order.
// It is a provenance identifier, not a confidence or semantic-correctness score.
func EvidenceID(source, prefix string, line int, text string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + prefix + strconv.Itoa(line) + "\x00" + text))
	return evidenceLabel(source, prefix) + ":" + strconv.Itoa(line) + "#" + hex.EncodeToString(sum[:2])
}

// evidenceLabel is the readable part of an ID: a source file's base name, or
// run- and the last digits of a run's output reference.
func evidenceLabel(source, prefix string) string {
	ref := strings.TrimSuffix(prefix, ":")
	switch source {
	case "git":
		if i := strings.Index(ref, ":"); i >= 0 {
			ref = ref[i+1:]
		}
		return path.Base(ref)
	case "replay":
		if n := strings.TrimPrefix(ref, "tool-"); n != ref && len(n) > 5 {
			return "run-" + n[len(n)-5:]
		}
	}
	return ref
}

type EvidenceLine struct {
	Line int    `json:"line"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

type NumberedDocument struct {
	Source string         `json:"source"`
	Prefix string         `json:"reference_prefix"`
	First  int            `json:"first_line"`
	Last   int            `json:"last_line"`
	Lines  []EvidenceLine `json:"lines"`
}

// Numbered exposes stable line IDs and actual bounds without manual line counting.
// The model sees this view; the audit retains the original raw lines.
func Numbered(d Document) NumberedDocument {
	v := NumberedDocument{Source: d.Source, Prefix: d.Prefix, First: d.First, Last: d.First + len(d.Lines) - 1, Lines: make([]EvidenceLine, 0, len(d.Lines))}
	for i, text := range d.Lines {
		line := d.First + i
		v.Lines = append(v.Lines, EvidenceLine{Line: line, ID: EvidenceID(d.Source, d.Prefix, line, text), Text: text})
	}
	return v
}
