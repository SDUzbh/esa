package replaytools

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"

	"esa/internal/esa"
)

var RoutingNames = []string{"issue_search", "issue_read"}

func (s *Server) issueCall(name string, args json.RawMessage) (map[string]any, error) {
	if s.Config.IssueDirectory == "" {
		return nil, errors.New("no issue directory is registered")
	}
	var a struct {
		Query  string `json:"query"`
		Kind   string `json:"kind"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return nil, err
	}
	directory, err := esa.OpenIssueDirectory(s.Config.IssuesRoot, s.Config.IssueDirectory, s.Config.IssueDirectoryHash)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	switch name {
	case "issue_search":
		if a.Limit == 0 {
			a.Limit = 10
		}
		rows, total, err := directory.Search(a.Query, a.Kind, a.Offset, a.Limit)
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
func routingTools() []map[string]any {
	str := func() map[string]string { return map[string]string{"type": "string"} }
	integer := func() map[string]string { return map[string]string{"type": "integer"} }
	return []map[string]any{
		{"name": "issue_search", "description": "Search the registered local Issue directory or browse with an empty query. Results are paginated metadata, not a decision of applicability. Prefer engineer_resolved records when equally applicable; inspect the record before reuse.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": str(), "kind": str(), "offset": integer(), "limit": integer()}, "required": []string{"query"}, "additionalProperties": false}},
		{"name": "issue_read", "description": "Read one frozen Issue record using the path and SHA256 returned by issue_search. Verify symptoms, versions, components and environment. A successful read is required before reusing a solution.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": str(), "sha256": str()}, "required": []string{"path", "sha256"}, "additionalProperties": false}},
	}
}
