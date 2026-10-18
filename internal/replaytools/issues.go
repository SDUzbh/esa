package replaytools

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"

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
		Query   string `json:"query"`
		Kind    string `json:"kind"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
		Archive bool   `json:"archive"`
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
func routingTools(withContext bool) []map[string]any {
	str := func() map[string]string { return map[string]string{"type": "string"} }
	integer := func() map[string]string { return map[string]string{"type": "integer"} }
	list := []map[string]any{
		{"name": "issue_search", "description": "Search the registered local Issue directory or browse with an empty query. Results are paginated metadata, not a decision of applicability. Prefer engineer_resolved records when equally applicable; inspect the record before reuse. archive=true searches only records of retired, solved Issues; use it when no current record applies.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": str(), "kind": str(), "offset": integer(), "limit": integer(), "archive": map[string]any{"type": "boolean"}}, "required": []string{"query"}, "additionalProperties": false}},
		{"name": "issue_read", "description": "Read one frozen Issue record using the path and SHA256 returned by issue_search. Verify symptoms, versions, components and environment. A successful read is required before reusing a solution.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": str(), "sha256": str()}, "required": []string{"path", "sha256"}, "additionalProperties": false}},
	}
	if withContext {
		list = append(list, map[string]any{"name": "issue_context", "description": "Read the Engineering Agent's grouping request and findings plus the earlier routing search trail. Reuse prior queries before doing any additional search.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}})
	}
	return list
}
