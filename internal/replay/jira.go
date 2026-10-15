// Package replay reconstructs a conservative ticket view at a historical cutoff.
package replay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"esa/internal/esa"
)

type Audit struct {
	Cutoff           time.Time `json:"cutoff"`
	ExcludedComments int       `json:"excluded_comments"`
	RewoundFields    []string  `json:"rewound_fields"`
	Limitations      []string  `json:"limitations"`
}

type change struct {
	Created string `json:"created"`
	Items   []struct {
		Field string  `json:"field"`
		From  *string `json:"fromString"`
		To    *string `json:"toString"`
	} `json:"items"`
}

// JiraAt accepts the preserved downloader envelope, never a current CSV snapshot.
// Edited comment bodies cannot be recovered and are excluded rather than leaked.
// JiraAt reconstructs a historical diagnostic ticket from complete Jira history.
// It excludes later comments, unavailable earlier edited bodies and resolution/fix evidence.
// Ambiguous or incomplete history returns an error instead of inventing past values.
func JiraAt(raw []byte, project string, at time.Time) (esa.Ticket, Audit, error) {
	audit := Audit{Cutoff: at, RewoundFields: []string{}, Limitations: []string{
		"The export has no independent initial field snapshot; completeness relies on its changelog metadata.",
		"Historical edits to comment bodies are unavailable; comments edited after the cutoff are excluded.",
		"Links, attachments, fix versions and resolution evidence are excluded from diagnostic input.",
	}}
	var envelope struct {
		Metadata struct {
			Complete bool `json:"history_complete"`
			Total    int  `json:"history_reported_total"`
		} `json:"metadata"`
		Issue     json.RawMessage `json:"issue"`
		Changelog []change        `json:"changelog"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return esa.Ticket{}, audit, err
	}
	if !envelope.Metadata.Complete || envelope.Metadata.Total != len(envelope.Changelog) {
		return esa.Ticket{}, audit, fmt.Errorf("historical replay requires a complete, count-checked changelog")
	}
	var issue struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(envelope.Issue, &issue); err != nil {
		return esa.Ticket{}, audit, err
	}
	if len(issue.Fields["created"]) == 0 || string(issue.Fields["created"]) == "null" || string(issue.Fields["created"]) == "\"\"" {
		return esa.Ticket{}, audit, fmt.Errorf("original creation timestamp is required")
	}
	if value := issue.Fields["security"]; len(value) > 0 && string(value) != "null" {
		return esa.Ticket{}, audit, fmt.Errorf("restricted Jira issues require dedicated replay access controls")
	}
	p, err := esa.ParseImport("issue.json", project, raw, at)
	if err != nil || len(p.Problems) > 0 || len(p.Tickets) != 1 {
		return esa.Ticket{}, audit, fmt.Errorf("invalid replay export: %v; %v", err, p.Problems)
	}
	t := p.Tickets[0]
	if at.IsZero() || t.CreatedAt.IsZero() || t.CreatedAt.After(at) {
		return esa.Ticket{}, audit, fmt.Errorf("cutoff must be at or after ticket creation")
	}
	times := make(map[int]time.Time)
	for i, h := range envelope.Changelog {
		ts, e := esa.ParseTime(h.Created)
		if e != nil || ts.IsZero() {
			return t, audit, fmt.Errorf("invalid changelog timestamp")
		}
		times[i] = ts
	}
	indices := make([]int, len(envelope.Changelog))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool { return times[indices[i]].After(times[indices[j]]) })
	for _, i := range indices {
		if !times[i].After(at) {
			continue
		}
		items := envelope.Changelog[i].Items
		for j := len(items) - 1; j >= 0; j-- {
			item := items[j]
			field := strings.ToLower(item.Field)
			old := ""
			if item.From != nil {
				old = *item.From
			}
			switch field {
			case "summary":
				t.Subject = old
			case "description":
				t.Description = old
			case "status":
				t.Status = old
			case "issuetype", "issue type":
				t.Tracker = old
			case "environment":
				t.Environment = map[string]string{}
				if old != "" {
					t.Environment["details"] = old
				}
			case "version", "versions", "affected version", "component", "components":
				xs := &t.Versions
				if strings.HasPrefix(field, "component") {
					xs = &t.Components
				}
				// Jira list items encode additions/removals separately. Multi-value display
				// strings are ambiguous; require a verified initial snapshot in that case.
				if strings.Contains(old, ",") || (item.To != nil && strings.Contains(*item.To, ",")) {
					return t, audit, fmt.Errorf("ambiguous multi-value history for %s", field)
				}
				next := []string{}
				for _, v := range *xs {
					if item.To == nil || v != *item.To {
						next = append(next, v)
					}
				}
				if old != "" {
					found := false
					for _, v := range next {
						if v == old {
							found = true
						}
					}
					if !found {
						next = append(next, old)
					}
				}
				*xs = next
			default:
				continue
			}
			audit.RewoundFields = append(audit.RewoundFields, field)
		}
	}
	comments := []esa.Comment{}
	for _, c := range t.Comments {
		if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() || c.CreatedAt.After(at) || c.UpdatedAt.After(at) {
			audit.ExcludedComments++
			continue
		}
		comments = append(comments, c)
	}
	t.Comments = comments
	t.UpdatedAt = at
	t.Assignee = ""
	t.Resolution = esa.Resolution{}
	t.StoredFields = nil
	t.Closed = strings.EqualFold(t.Status, "closed") || strings.EqualFold(t.Status, "resolved") || strings.EqualFold(t.Status, "done")
	t.Source = "historical_replay"
	return t, audit, nil
}
