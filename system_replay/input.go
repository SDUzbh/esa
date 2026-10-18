package systemreplay

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func clone(v any) any     { b, _ := json.Marshal(v); var out any; _ = json.Unmarshal(b, &out); return out }
func names(v any) []any {
	out := []any{}
	xs, _ := v.([]any)
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok {
			if name := text(m["name"]); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}
func jira(raw []byte, job Job) (map[string]any, string, []string, error) {
	var envelope struct {
		Metadata struct {
			Complete bool `json:"history_complete"`
			Total    int  `json:"history_reported_total"`
		} `json:"metadata"`
		Issue struct {
			Key    string         `json:"key"`
			Fields map[string]any `json:"fields"`
		} `json:"issue"`
		Changelog []struct {
			Created string `json:"created"`
			Items   []struct {
				Field string  `json:"field"`
				From  *string `json:"fromString"`
				To    *string `json:"toString"`
			} `json:"items"`
		} `json:"changelog"`
	}
	if e := json.Unmarshal(raw, &envelope); e != nil {
		return nil, "", nil, e
	}
	if !envelope.Metadata.Complete || envelope.Metadata.Total != len(envelope.Changelog) {
		return nil, "", nil, fmt.Errorf("historical Jira input requires count-checked complete history")
	}
	f := envelope.Issue.Fields
	if f["security"] != nil {
		return nil, "", nil, fmt.Errorf("restricted Jira input is not supported")
	}
	at := job.At
	if at == "" {
		at = text(f["created"])
	}
	cutoff, e := timestamp(at)
	if e != nil {
		return nil, "", nil, fmt.Errorf("valid observation time required")
	}
	created, e := timestamp(text(f["created"]))
	if e != nil || created.After(cutoff) {
		return nil, "", nil, fmt.Errorf("observation precedes creation")
	}
	named := func(key string) string { m, _ := f[key].(map[string]any); return text(m["name"]) }
	environment := map[string]any{}
	if s := text(f["environment"]); s != "" {
		environment["details"] = s
	}
	t := map[string]any{"id": envelope.Issue.Key, "source_key": envelope.Issue.Key, "project_id": job.Project, "subject": f["summary"], "description": f["description"], "tracker": named("issuetype"), "status": named("status"), "source": "historical_replay", "components": names(f["components"]), "versions": names(f["versions"]), "environment": environment, "error_codes": []any{}, "comments": []any{}, "created_at": created.Format(time.RFC3339Nano), "updated_at": cutoff.Format(time.RFC3339Nano), "resolution": map[string]any{}}
	indices := make([]int, len(envelope.Changelog))
	dates := map[int]time.Time{}
	for i, h := range envelope.Changelog {
		ts, e := timestamp(h.Created)
		if e != nil {
			return nil, "", nil, e
		}
		indices[i] = i
		dates[i] = ts
	}
	sort.SliceStable(indices, func(i, j int) bool { return dates[indices[i]].After(dates[indices[j]]) })
	for _, i := range indices {
		if !dates[i].After(cutoff) {
			continue
		}
		items := envelope.Changelog[i].Items
		for n := len(items) - 1; n >= 0; n-- {
			item := items[n]
			old := ""
			if item.From != nil {
				old = *item.From
			}
			field := strings.ToLower(item.Field)
			switch field {
			case "summary":
				t["subject"] = old
			case "description", "status":
				t[field] = old
			case "issuetype", "issue type":
				t["tracker"] = old
			case "environment":
				environment = map[string]any{}
				if old != "" {
					environment["details"] = old
				}
				t["environment"] = environment
			case "version", "versions", "affected version", "component", "components":
				if strings.Contains(old, ",") || (item.To != nil && strings.Contains(*item.To, ",")) {
					return nil, "", nil, fmt.Errorf("ambiguous Jira list history: %s", field)
				}
				key := "versions"
				if strings.HasPrefix(field, "component") {
					key = "components"
				}
				list, _ := t[key].([]any)
				next := []any{}
				for _, v := range list {
					if item.To == nil || text(v) != *item.To {
						next = append(next, v)
					}
				}
				if old != "" {
					exists := false
					for _, v := range next {
						if text(v) == old {
							exists = true
						}
					}
					if !exists {
						next = append(next, old)
					}
				}
				t[key] = next
			}
		}
	}
	cm, _ := f["comment"].(map[string]any)
	comments, _ := cm["comments"].([]any)
	for _, value := range comments {
		c, _ := value.(map[string]any)
		ct, e1 := timestamp(text(c["created"]))
		ut, e2 := timestamp(text(c["updated"]))
		if e1 != nil || e2 != nil || ct.After(cutoff) || ut.After(cutoff) {
			continue
		}
		t["comments"] = append(t["comments"].([]any), map[string]any{"id": fmt.Sprint(c["id"]), "body": c["body"], "created_at": ct.Format(time.RFC3339Nano), "updated_at": ut.Format(time.RFC3339Nano)})
	}
	status := strings.ToLower(text(t["status"]))
	t["closed"] = status == "closed" || status == "resolved" || status == "done"
	t["private"] = false
	return t, cutoff.Format(time.RFC3339Nano), []string{"Historical input reconstruction relies on the exported changelog; edited historical bodies are unavailable.", "Links, attachments, fix versions and resolution fields are not sent as initial evidence."}, nil
}
func preparePlan(ctx context.Context, m Manifest, j Job) (Plan, error) {
	p := Plan{Job: j, Version: j.Version, At: j.At, Limitations: []string{}}
	if j.Raw != "" {
		b, e := os.ReadFile(j.Raw)
		if e != nil {
			return p, e
		}
		p.RawHash = sha(b)
		p.Ticket, p.At, p.Limitations, e = jira(b, j)
		if e != nil {
			return p, e
		}
	} else {
		if j.Ticket == nil {
			return p, fmt.Errorf("job needs raw Jira input or an explicit ticket")
		}
		p.Ticket = clone(j.Ticket).(map[string]any)
		if p.At == "" {
			p.At = text(p.Ticket["updated_at"])
		}
		p.Limitations = append(p.Limitations, "Explicit ticket is operator-supplied; its historical provenance is not reconstructed.")
	}
	if _, e := timestamp(p.At); e != nil {
		return p, e
	}
	if text(p.Ticket["id"]) == "" {
		return p, fmt.Errorf("ticket id is required")
	}
	if text(p.Ticket["project_id"]) == "" {
		p.Ticket["project_id"] = j.Project
	}
	if len(j.Environment) > 0 {
		env := map[string]any{}
		for k, v := range j.Environment {
			env[k] = v
		}
		p.Ticket["environment"] = env
	}
	if m.Repo != "" {
		ref := j.Ref
		if ref == "" {
			versions, _ := p.Ticket["versions"].([]any)
			if len(versions) != 1 {
				return p, fmt.Errorf("an explicit historical ref or single affected version is required")
			}
			ref = "refs/tags/" + text(versions[0])
		}
		g := Git{m.Repo, ctx}
		id, e := g.resolve(ref)
		if e != nil {
			return p, e
		}
		p.Commit = id
		stamps, e := g.run("show", "-s", "--format=%aI%n%cI", id)
		if e != nil {
			return p, e
		}
		at, _ := timestamp(p.At)
		for _, stamp := range strings.Fields(stamps) {
			t, e := timestamp(stamp)
			if e != nil || t.After(at) {
				return p, fmt.Errorf("historical source is after observation")
			}
		}
		if p.Version == "" {
			p.Version = strings.TrimPrefix(ref, "refs/tags/")
		}
	}
	if p.Version == "" {
		p.Version = "0.0"
	}
	if !releaseTag.MatchString(p.Version) {
		return p, fmt.Errorf("explicit numeric version label required")
	}
	last, _ := timestamp(p.At)
	for _, event := range j.Events {
		at, e := timestamp(event.At)
		if e != nil || at.Before(last) {
			return p, fmt.Errorf("job events must be chronological and not precede baseline")
		}
		last = at
	}
	if j.Scenario != nil {
		if m.Repo == "" || len(m.Runtime.Args) == 0 {
			return p, fmt.Errorf("scenario requires product repository and independent runtime driver")
		}
		if j.Scenario.Profile == "source" {
			return p, fmt.Errorf("source readiness cannot establish a defect replay")
		}
		if len(j.Scenario.Controls) == 0 || len(j.Scenario.Bug) == 0 || len(j.Scenario.Healthy) == 0 {
			return p, fmt.Errorf("scenario requires control, failure and repaired-behavior assertions")
		}
	}
	return p, nil
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func safe(s string) string { return safeName.ReplaceAllString(s, "_") }
func export(ctx context.Context, repo, commit, root string) (_ string, hashes map[string]string, err error) {
	if !regexp.MustCompile(`^[0-9a-f]{40,64}$`).MatchString(commit) {
		return "", nil, fmt.Errorf("export requires immutable Git SHA")
	}
	directory := filepath.Join(root, "snapshots", commit)
	marker := filepath.Join(directory, "manifest.json")
	if b, e := os.ReadFile(marker); e == nil {
		var hashes map[string]string
		if e = json.Unmarshal(b, &hashes); e != nil {
			return "", nil, e
		}
		for path, h := range hashes {
			b, e := os.ReadFile(filepath.Join(directory, "source", path))
			if e != nil || sha(b) != h {
				return "", nil, fmt.Errorf("snapshot changed: %s", path)
			}
		}
		return filepath.Join(directory, "source"), hashes, nil
	}
	// Without its manifest a snapshot is incomplete, and its read-only files
	// would block the next export of this commit; start over and clean up.
	if e := os.RemoveAll(directory); e != nil {
		return "", nil, e
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	if e := os.MkdirAll(directory, 0700); e != nil {
		return "", nil, e
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "archive", "--format=tar", commit)
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		return "", nil, e
	}
	if e = cmd.Start(); e != nil {
		return "", nil, e
	}
	hashes = map[string]string{}
	tr := tar.NewReader(pipe)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", nil, err
		}
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}
		name := filepath.Clean(header.Name)
		if header.Typeflag != tar.TypeReg || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", nil, fmt.Errorf("unsafe source archive entry: %s", name)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", nil, err
		}
		path := filepath.Join(directory, "source", name)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", nil, err
		}
		if err = os.WriteFile(path, b, 0400); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", nil, err
		}
		hashes[filepath.ToSlash(name)] = sha(b)
	}
	if e = cmd.Wait(); e != nil {
		return "", nil, e
	}
	if e = write(marker, hashes); e != nil {
		return "", nil, e
	}
	return filepath.Join(directory, "source"), hashes, nil
}
func write(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	temporary := path + ".tmp"
	if e = os.WriteFile(temporary, append(b, '\n'), 0600); e != nil {
		return e
	}
	return os.Rename(temporary, path)
}
func equal(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }
