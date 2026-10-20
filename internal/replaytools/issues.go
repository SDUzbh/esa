package replaytools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"esa/internal/esa"
)

var RoutingNames = []string{"issue_search", "issue_read", "issue_context"}

// A directory-bound local handoff is read only by Routing Agent. Engineering
// contributes its request/findings; no archive is supplied to its tool server.
type IssueContext struct {
	Ticket        *esa.Ticket       `json:"ticket,omitempty"`
	DirectoryHash string            `json:"directory_hash"`
	Reason        string            `json:"reason"`
	Queries       []Record          `json:"queries"`
	Request       *esa.IssueRequest `json:"issue_request,omitempty"`
	Findings      []esa.Finding     `json:"findings,omitempty"`
	Limitations   []string          `json:"limitations,omitempty"`
}

func (s *Server) issueCall(name string, args json.RawMessage) (map[string]any, error) {
	if s.Config.IssueDirectory == "" {
		return nil, errors.New("no issue directory is registered")
	}
	var a struct {
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
		Kind    string   `json:"kind"`
		Offset  int      `json:"offset"`
		Limit   int      `json:"limit"`
		Path    string   `json:"path"`
		SHA256  string   `json:"sha256"`
		Archive bool     `json:"archive"`
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return nil, err
	}
	if s.issueDirectory == nil {
		view, err := esa.OpenIssueDirectory(s.Config.IssuesRoot, s.Config.IssueDirectory, s.Config.IssueDirectoryHash)
		if err != nil {
			return nil, err
		}
		s.issueDirectory = &view
	}
	directory := s.issueDirectory
	var err error
	result := map[string]any{}
	switch name {
	case "issue_context":
		if s.Config.IssueContext == "" {
			return nil, errors.New("no previous routing search is registered")
		}
		data, err := os.ReadFile(s.Config.IssueContext)
		if err != nil {
			return nil, err
		}
		var context IssueContext
		if err = json.Unmarshal(data, &context); err != nil {
			return nil, err
		}
		if context.DirectoryHash != s.Config.IssueDirectoryHash {
			return nil, errors.New("retrieval context belongs to another directory")
		}
		result = map[string]any{"routing_context": context}
	case "issue_search":
		if a.Limit == 0 {
			a.Limit = 10
		}
		search := directory.Search
		if a.Archive {
			search = directory.SearchArchive
		}
		if a.Queries != nil {
			if a.Query != "" {
				return nil, errors.New("pass either query or queries, not both")
			}
			if result, err = searchMany(search, a.Queries, a.Kind, a.Offset, a.Limit); err != nil {
				return nil, err
			}
			break
		}
		rows, total, err := search(a.Query, a.Kind, a.Offset, a.Limit)
		if err != nil {
			return nil, err
		}
		result = map[string]any{"entries": rows, "total": total, "next_offset": a.Offset + len(rows)}
	case "issue_read":
		if a.SHA256 == "" {
			return nil, errors.New("use the SHA256 from issue_search")
		}
		record, err := directory.ReadRecord(a.Path, a.SHA256)
		if err != nil {
			return nil, err
		}
		result = map[string]any{"record": record, "path": a.Path, "sha256": a.SHA256}
	default:
		return nil, errors.New("only issue_search and issue_read are available to Routing Agent")
	}
	// Log identity and successful reads, not a duplicate archive of every solution.
	audit := Record{Tool: name, Arguments: args}
	if name == "issue_search" {
		audit.Result = result
	}
	raw, _ := json.Marshal(audit)
	f, err := os.OpenFile(s.Config.AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(append(raw, '\n'))
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	return result, closeErr
}

// maxQueries bounds one batched issue_search.
const maxQueries = 5

// batchEntry is a hit of a batched search with the queries that found it.
type batchEntry struct {
	esa.IssueIndexEntry
	Matched []string `json:"matched_queries"`
}

// searchMany runs several first-page searches in one call, so the Routing
// Agent can try its symptom, exception, component and version wordings
// without a model call per query. Hits are merged by their best rank in any
// query, then by query order; each lists the queries that found it.
func searchMany(search func(string, string, int, int) ([]esa.IssueIndexEntry, int, error), queries []string, kind string, offset, limit int) (map[string]any, error) {
	if len(queries) == 0 || len(queries) > maxQueries || offset != 0 {
		return nil, fmt.Errorf("queries takes 1..%d searches and no offset; page one query with query and offset", maxQueries)
	}
	type hit struct {
		entry      batchEntry
		rank, from int
	}
	hits := map[string]*hit{}
	totals := map[string]int{}
	for i, q := range queries {
		if strings.TrimSpace(q) == "" {
			return nil, errors.New("each batched query must be non-empty; browse with an empty query")
		}
		if _, repeated := totals[q]; repeated {
			continue
		}
		rows, total, err := search(q, kind, 0, limit)
		if err != nil {
			return nil, err
		}
		totals[q] = total
		for rank, row := range rows {
			h := hits[row.Path]
			if h == nil {
				h = &hit{entry: batchEntry{IssueIndexEntry: row}, rank: rank, from: i}
				hits[row.Path] = h
			}
			h.entry.Matched = append(h.entry.Matched, q)
			h.rank = min(h.rank, rank)
		}
	}
	merged := make([]*hit, 0, len(hits))
	for _, h := range hits {
		merged = append(merged, h)
	}
	sort.Slice(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.from != b.from {
			return a.from < b.from
		}
		return a.entry.Path < b.entry.Path
	})
	entries := make([]batchEntry, 0, len(merged))
	for _, h := range merged {
		entries = append(entries, h.entry)
	}
	return map[string]any{"entries": entries, "totals": totals}, nil
}

func routingTools(withContext bool) []map[string]any {
	str := func() map[string]string { return map[string]string{"type": "string"} }
	integer := func() map[string]string { return map[string]string{"type": "integer"} }
	list := []map[string]any{
		{
			"name": "issue_search",
			"description": "Search the registered local Issue directory or browse with an empty query. Results are " +
				"paginated metadata, not a decision of applicability. Pass queries (up to 5) instead of query to " +
				"run several wordings in one call; their first pages are merged and each hit lists the " +
				"queries that found it. Prefer engineer_resolved records " +
				"when equally applicable; inspect the record before reuse. archive=true searches only " +
				"records of retired, solved Issues; use it when no current record applies.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":   str(),
					"queries": map[string]any{"type": "array", "items": str(), "maxItems": maxQueries},
					"kind":    str(),
					"offset":  integer(),
					"limit":   integer(),
					"archive": map[string]any{"type": "boolean"},
				},
				"additionalProperties": false,
			},
		},
		{
			"name": "issue_read",
			"description": "Read one frozen Issue record using the path and SHA256 returned by issue_search. Verify " +
				"symptoms, versions, components and environment. A successful read is required before " +
				"reusing a solution.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"path": str(), "sha256": str()},
				"required":             []string{"path", "sha256"},
				"additionalProperties": false,
			},
		},
	}
	if withContext {
		list = append(list, map[string]any{
			"name": "issue_context",
			"description": "Read the Engineering Agent's grouping request and findings plus the earlier routing " +
				"search trail. Reuse prior queries before doing any additional search.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		})
	}
	return list
}
