package esa

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type ImportProblem struct {
	Record  int    `json:"record"`
	Message string `json:"message"`
}
type Preview struct {
	Tickets  []Ticket        `json:"tickets"`
	Problems []ImportProblem `json:"problems"`
	Warnings []string        `json:"warnings"`
}

func ParseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05-0700",
		"2006-01-02 15:04:05 -0700",
		"2/Jan/06 3:04 PM",
		"02/Jan/06 15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported source timestamp %q; use an ISO 8601 timestamp with a timezone", value)
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	b, _ := json.Marshal(value)
	return string(b)
}
func object(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
func named(value any) string { return stringValue(object(value)["name"]) }
func names(value any) []string {
	out := []string{}
	if xs, ok := value.([]any); ok {
		for _, x := range xs {
			if s := named(x); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func parseJira(record map[string]any) (Ticket, error) {
	issue := record
	if m, ok := record["issue"].(map[string]any); ok {
		issue = m
	}
	f := object(issue["fields"])
	t := Ticket{
		SourceKey:   stringValue(issue["key"]),
		Subject:     stringValue(f["summary"]),
		Description: stringValue(f["description"]),
		Tracker:     named(f["issuetype"]),
		Status:      named(f["status"]),
		Source:      "manual_upload",
		Components:  names(f["components"]),
		Versions:    names(f["versions"]),
		Environment: map[string]string{},
	}
	t.ID = t.SourceKey
	t.Resolution.Reason = named(f["resolution"])
	t.Closed = contains([]string{"Resolved", "Closed", "Done"}, t.Status)
	if environment := stringValue(f["environment"]); environment != "" {
		t.Environment["details"] = environment
	}
	var err error
	if t.CreatedAt, err = ParseTime(stringValue(f["created"])); err != nil {
		return t, err
	}
	if t.UpdatedAt, err = ParseTime(stringValue(f["updated"])); err != nil {
		return t, err
	}
	comments, ok := record["comments"].([]any)
	if !ok {
		comments, _ = object(f["comment"])["comments"].([]any)
	}
	for _, value := range comments {
		c := object(value)
		created, err := ParseTime(stringValue(c["created"]))
		if err != nil {
			return t, err
		}
		updated, err := ParseTime(stringValue(c["updated"]))
		if err != nil {
			return t, err
		}
		t.Comments = append(t.Comments, Comment{ID: stringValue(c["id"]), Body: stringValue(c["body"]), CreatedAt: created, UpdatedAt: updated})
	}
	return t, nil
}

func ParseImport(filename, projectID string, data []byte, received time.Time) (Preview, error) {
	preview := Preview{Tickets: []Ticket{}, Problems: []ImportProblem{}, Warnings: []string{}}
	if projectID == "" {
		return preview, fmt.Errorf("%w: select a project", ErrInvalid)
	}
	if !utf8.Valid(data) {
		return preview, fmt.Errorf("%w: upload a UTF-8 encoded file", ErrInvalid)
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	appendTicket := func(t Ticket, index int, err error) {
		if err != nil {
			preview.Problems = append(preview.Problems, ImportProblem{index, err.Error()})
			return
		}
		if strings.TrimSpace(t.Subject) == "" {
			preview.Problems = append(preview.Problems, ImportProblem{index, "Subject is required."})
			return
		}
		t.ProjectID = projectID
		t.Source = "manual_upload"
		if t.ID == "" {
			t.ID = "upload_" + hash(t)[:24]
		}
		if t.SourceKey == "" {
			t.SourceKey = t.ID
		}
		if t.UpdatedAt.IsZero() {
			t.UpdatedAt = received
			preview.Warnings = append(preview.Warnings, fmt.Sprintf("Record %d has no update timestamp. Receiving time will be used; this upload is not a historical replay snapshot.", index))
		}
		if t.CreatedAt.IsZero() {
			t.CreatedAt = t.UpdatedAt
		}
		preview.Tickets = append(preview.Tickets, t)
	}
	switch lower(filepath.Ext(filename)) {
	case ".json":
		var payload any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			return preview, fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return preview, fmt.Errorf("%w: expected one JSON document", ErrInvalid)
		}
		var records []any
		if xs, ok := payload.([]any); ok {
			records = xs
		} else if xs, ok := object(payload)["issues"].([]any); ok {
			records = xs
		} else {
			records = []any{payload}
		}
		if len(records) > 2000 {
			return preview, fmt.Errorf("%w: at most 2000 tickets per upload", ErrInvalid)
		}
		for i, value := range records {
			record, ok := value.(map[string]any)
			if !ok {
				appendTicket(Ticket{}, i+1, fmt.Errorf("Record must be an object."))
				continue
			}
			issue := object(record["issue"])
			if record["fields"] != nil || issue["fields"] != nil {
				t, err := parseJira(record)
				appendTicket(t, i+1, err)
			} else {
				if record["ticket"] != nil {
					record = object(record["ticket"])
				}
				b, _ := json.Marshal(record)
				var t Ticket
				err := json.Unmarshal(b, &t)
				appendTicket(t, i+1, err)
			}
		}
	case ".csv":
		r := csv.NewReader(bytes.NewReader(data))
		r.FieldsPerRecord = -1
		headers, err := r.Read()
		if err != nil {
			return preview, fmt.Errorf("%w: invalid CSV header", ErrInvalid)
		}
		for i := range headers {
			headers[i] = lower(headers[i])
		}
		for index := 1; ; index++ {
			row, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return preview, fmt.Errorf("%w: CSV record %d: %v", ErrInvalid, index, err)
			}
			if index > 2000 {
				return preview, fmt.Errorf("%w: at most 2000 tickets per upload", ErrInvalid)
			}
			if len(row) != len(headers) {
				appendTicket(Ticket{}, index, fmt.Errorf("Column count does not match the header."))
				continue
			}
			values := map[string][]string{}
			for i, key := range headers {
				values[key] = append(values[key], row[i])
			}
			get := func(keys ...string) string {
				for _, key := range keys {
					for _, value := range values[key] {
						if value != "" {
							return value
						}
					}
				}
				return ""
			}
			many := func(keys ...string) []string {
				out := []string{}
				for _, key := range keys {
					for _, value := range values[key] {
						out = append(out, strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == '|' })...)
					}
				}
				return unique(out)
			}
			t := Ticket{
				ID:          get("id", "issue key", "key"),
				SourceKey:   get("issue key", "key", "id"),
				Subject:     get("subject", "summary"),
				Description: get("description"),
				Tracker:     get("tracker", "issue type"),
				Status:      get("status"),
				Assignee:    get("assignee", "assigned to"),
				Components:  many("components", "component/s"),
				Versions:    many("versions", "affects version/s", "affected versions"),
				Environment: map[string]string{},
			}
			if environment := get("environment"); environment != "" {
				t.Environment["details"] = environment
			}
			t.Closed = contains([]string{"Resolved", "Closed", "Done"}, t.Status)
			t.Resolution = Resolution{
				Reason:     get("resolution"),
				Cause:      get("cause", "root cause"),
				Solution:   get("solution"),
				Validation: get("validation", "verification"),
			}
			for j, body := range values["comment"] {
				if body != "" {
					t.Comments = append(t.Comments, Comment{ID: fmt.Sprint(j + 1), Body: body})
				}
			}
			t.CreatedAt, err = ParseTime(get("created", "created_at"))
			if err == nil {
				t.UpdatedAt, err = ParseTime(get("updated", "updated_at"))
			}
			appendTicket(t, index, err)
		}
	default:
		return preview, fmt.Errorf("%w: supported file types are .json and .csv", ErrInvalid)
	}
	return preview, nil
}
