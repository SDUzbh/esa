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

// Upload bounds. A real Jira record with its comments and changelog reaches
// about 4.3 MB and 38,000 JSON values in the Kafka export; Jira CSV exports
// repeat comment columns. Decoding JSON into generic values costs about 40
// times its size for tiny values, so records are decoded one at a time and
// each is checked before decoding.
const (
	maxImportRecords      = 2000
	maxImportRecordBytes  = 8 << 20
	maxImportRecordValues = 200_000
	maxImportCSVColumns   = 1024
	maxImportCSVField     = 8 << 20
)

// jsonRecords calls each with every record of an upload: the elements of a
// top-level array or of an "issues" array, or the document itself. Arrays
// are read one element at a time and the record cap is applied while
// reading, so an oversized upload is rejected without decoding all of it.
// A record over the size or value bound is reported as that record's
// problem instead of being decoded.
func jsonRecords(data []byte, each func(index int, value any, problem error)) error {
	decodeRecord := func(index int, raw []byte) {
		if len(raw) > maxImportRecordBytes {
			each(index, nil, fmt.Errorf("Record is larger than %d MB.", maxImportRecordBytes>>20))
			return
		}
		if jsonValues(raw) > maxImportRecordValues {
			each(index, nil, fmt.Errorf("Record has more than %d JSON values.", maxImportRecordValues))
			return
		}
		var value any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&value); err != nil {
			each(index, nil, err)
			return
		}
		each(index, value, nil)
	}
	array := func(raw []byte) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		if _, err := d.Token(); err != nil {
			return fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
		}
		for index := 1; d.More(); index++ {
			if index > maxImportRecords {
				return fmt.Errorf("%w: at most %d tickets per upload", ErrInvalid, maxImportRecords)
			}
			var element json.RawMessage
			if err := d.Decode(&element); err != nil {
				return fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
			}
			decodeRecord(index, element)
		}
		if _, err := d.Token(); err != nil {
			return fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
		}
		return expectEnd(d)
	}
	switch firstByte(data) {
	case '[':
		return array(data)
	case '{':
		// Only "issues" is copied out; the rest is validated in place.
		var top struct {
			Issues json.RawMessage `json:"issues"`
		}
		if err := json.Unmarshal(data, &top); err != nil {
			return fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
		}
		if firstByte(top.Issues) == '[' {
			return array(top.Issues)
		}
		decodeRecord(1, bytes.TrimSpace(data))
		return nil
	default:
		var value any
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		if err := d.Decode(&value); err != nil {
			return fmt.Errorf("%w: invalid JSON: %v", ErrInvalid, err)
		}
		if err := expectEnd(d); err != nil {
			return err
		}
		each(1, value, nil)
		return nil
	}
}

func expectEnd(d *json.Decoder) error {
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%w: expected one JSON document", ErrInvalid)
	}
	return nil
}

func firstByte(data []byte) byte {
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) > 0 {
		return trimmed[0]
	}
	return 0
}

// jsonValues counts the values in a JSON text without allocating: one per
// object or array, plus one per element separator, plus the first value.
func jsonValues(raw []byte) int {
	n, inString, escaped := 1, false, false
	for _, c := range raw {
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[' || c == ',':
			n++
		}
	}
	return n
}

// csvMaxColumns returns the largest number of columns in any CSV row, without
// allocating, and stops counting past maxImportCSVColumns. A quote toggles
// quoted text; a doubled quote inside it toggles twice and stays quoted.
func csvMaxColumns(data []byte) int {
	most, columns, quoted := 0, 1, false
	for _, c := range data {
		switch {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == ',':
			columns++
		case c == '\n':
			most, columns = max(most, columns), 1
		}
		if columns > maxImportCSVColumns {
			return columns
		}
	}
	return max(most, columns)
}

func oversizedField(fields []string) bool {
	for _, f := range fields {
		if len(f) > maxImportCSVField {
			return true
		}
	}
	return false
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
		err := jsonRecords(data, func(index int, value any, problem error) {
			if problem != nil {
				appendTicket(Ticket{}, index, problem)
				return
			}
			record, ok := value.(map[string]any)
			if !ok {
				appendTicket(Ticket{}, index, fmt.Errorf("Record must be an object."))
				return
			}
			issue := object(record["issue"])
			if record["fields"] != nil || issue["fields"] != nil {
				t, err := parseJira(record)
				appendTicket(t, index, err)
			} else {
				if record["ticket"] != nil {
					record = object(record["ticket"])
				}
				b, _ := json.Marshal(record)
				var t Ticket
				err := json.Unmarshal(b, &t)
				appendTicket(t, index, err)
			}
		})
		if err != nil {
			return preview, err
		}
	case ".csv":
		// csv.Reader allocates every field of a row before it can be counted,
		// so the column bound is applied to the raw text first.
		if columns := csvMaxColumns(data); columns > maxImportCSVColumns {
			return preview, fmt.Errorf("%w: a CSV row has more than %d columns", ErrInvalid, maxImportCSVColumns)
		}
		r := csv.NewReader(bytes.NewReader(data))
		r.FieldsPerRecord = -1
		headers, err := r.Read()
		if err != nil {
			return preview, fmt.Errorf("%w: invalid CSV header", ErrInvalid)
		}
		if oversizedField(headers) {
			return preview, fmt.Errorf("%w: a CSV header is larger than %d MB", ErrInvalid, maxImportCSVField>>20)
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
			if oversizedField(row) {
				appendTicket(Ticket{}, index, fmt.Errorf("A field is larger than %d MB.", maxImportCSVField>>20))
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
