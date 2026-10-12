// Package classifier implements an optional, human-supervised multilabel model.
// It has no dependency on ESA routing or engineering agents.
package classifier

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
)

type Example struct {
	ID         string    `json:"id"`
	GroupID    string    `json:"group_id,omitempty"`
	ProjectID  string    `json:"project_id"`
	Text       string    `json:"text"`
	Labels     []string  `json:"labels"`
	Reviewer   string    `json:"reviewer"`
	ReviewedAt time.Time `json:"reviewed_at"`
}

// ReadExamples accepts strict JSONL, not inferred labels or resolved outcomes.
func ReadExamples(r io.Reader) ([]Example, error) {
	limited := &io.LimitedReader{R: r, N: (256 << 20) + 1}
	s := bufio.NewScanner(limited)
	s.Buffer(make([]byte, 4096), 2<<20)
	var out []Example
	for line := 1; s.Scan(); line++ {
		if strings.TrimSpace(s.Text()) == "" {
			continue
		}
		var e Example
		d := json.NewDecoder(strings.NewReader(s.Text()))
		d.DisallowUnknownFields()
		if err := d.Decode(&e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		var trailing any
		if d.Decode(&trailing) != io.EOF {
			return nil, fmt.Errorf("line %d: expected one JSON value", line)
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, e)
		if len(out) > 100000 {
			return nil, fmt.Errorf("dataset exceeds 100,000 examples")
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("dataset exceeds 256 MB")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dataset is empty")
	}
	return out, nil
}

func (e Example) Validate() error {
	if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.ProjectID) == "" || len(e.Text) > 1<<20 || len(terms(e.Text)) == 0 || strings.TrimSpace(e.Reviewer) == "" || e.ReviewedAt.IsZero() || e.Labels == nil {
		return fmt.Errorf("id, project_id, tokenizable text, explicit labels, reviewer and reviewed_at are required")
	}
	seen := map[string]bool{}
	for _, label := range e.Labels {
		if label == "" || label != strings.TrimSpace(label) || len(label) > 100 || seen[label] {
			return fmt.Errorf("invalid or duplicate label %q", label)
		}
		seen[label] = true
	}
	return nil
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func GroupKey(e Example) string {
	g := e.GroupID
	if g == "" {
		g = e.ID
	}
	return digest([]string{e.ProjectID, g})
}
func TextHash(text string) string { return digest(terms(text)) }

// Sorted counts make fitting and inference deterministic. Word bigrams preserve
// some local context; this tokenizer does not perform language segmentation.
func terms(text string) map[string]int {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
	t := map[string]int{}
	for i, w := range words {
		t[w]++
		if i > 0 {
			t[words[i-1]+" "+w]++
		}
	}
	return t
}
func sortedKeys[V any](m map[string]V) []string {
	x := make([]string, 0, len(m))
	for k := range m {
		x = append(x, k)
	}
	sort.Strings(x)
	return x
}

// MergeFeedback replaces the previous human label set for a ticket with its
// newest correction at the cutoff. Future reviews are never included.
func MergeFeedback(base, feedback []Example, cutoff time.Time) ([]Example, error) {
	if cutoff.IsZero() {
		return nil, fmt.Errorf("a training cutoff is required")
	}
	latest := map[string]Example{}
	for _, batch := range [][]Example{base, feedback} {
		for _, e := range batch {
			if err := e.Validate(); err != nil {
				return nil, err
			}
			if e.ReviewedAt.After(cutoff) {
				continue
			}
			key := digest([]string{e.ProjectID, e.ID})
			if old, ok := latest[key]; ok {
				if e.ReviewedAt.Equal(old.ReviewedAt) && digest(e) != digest(old) {
					return nil, fmt.Errorf("conflicting reviews at the same timestamp for %s", e.ID)
				}
				if e.ReviewedAt.Before(old.ReviewedAt) {
					continue
				}
				// Preserve the original grouping, so corrections cannot cross the test boundary.
				if GroupKey(e) != GroupKey(old) {
					return nil, fmt.Errorf("group changed for %s", e.ID)
				}
			}
			latest[key] = e
		}
	}
	out := make([]Example, 0, len(latest))
	for _, k := range sortedKeys(latest) {
		out = append(out, latest[k])
	}
	return out, nil
}
