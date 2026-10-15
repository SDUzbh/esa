package replaytools

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// EvidenceID binds a selected line to its source, reference and exact bytes.
// Consumers must reject ambiguous IDs rather than resolving a collision by order.
func EvidenceID(source, prefix string, line int, text string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + prefix + strconv.Itoa(line) + "\x00" + text))
	return "e_" + hex.EncodeToString(sum[:10])
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

// Numbered is the model-facing view; the audit retains the original raw lines.
func Numbered(d Document) NumberedDocument {
	v := NumberedDocument{Source: d.Source, Prefix: d.Prefix, First: d.First, Last: d.First + len(d.Lines) - 1, Lines: make([]EvidenceLine, 0, len(d.Lines))}
	for i, text := range d.Lines {
		line := d.First + i
		v.Lines = append(v.Lines, EvidenceLine{Line: line, ID: EvidenceID(d.Source, d.Prefix, line, text), Text: text})
	}
	return v
}
