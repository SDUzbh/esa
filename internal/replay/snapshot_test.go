package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHistoricalVersionSelectionAndExport(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(date string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Replay Test", "GIT_AUTHOR_EMAIL=replay@example.test", "GIT_COMMITTER_NAME=Replay Test", "GIT_COMMITTER_EMAIL=replay@example.test", "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return string(out)
	}
	old := "2015-11-01T00:00:00Z"
	git(old, "init", "-q")
	content := []byte("historical implementation\n")
	os.WriteFile(filepath.Join(repo, "Code.java"), content, 0600)
	git(old, "add", "Code.java")
	git(old, "commit", "-qm", "historical")
	git(old, "tag", "0.9.0.0")
	git("2015-12-01T00:00:00Z", "commit", "--allow-empty", "-qm", "future")
	git(old, "tag", "0.9.0.1")
	raw := filepath.Join(dir, "ticket.json")
	writeTicket := func(versions []any) {
		b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"history_complete": true, "history_reported_total": 0}, "issue": map[string]any{"key": "KAFKA-X", "fields": map[string]any{"summary": "Consumer failure", "description": "Observed exception", "issuetype": map[string]string{"name": "Bug"}, "status": map[string]string{"name": "Open"}, "created": "2015-11-24T00:13:27.387Z", "versions": versions}}, "changelog": []any{}})
		if err := os.WriteFile(raw, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	version := func(name string) any { return map[string]string{"name": name} }
	writeTicket([]any{version("0.9.0.0")})
	p, err := Resolve(context.Background(), raw, repo, "", "", "1")
	if err != nil || p.Ref != "0.9.0.0" {
		t.Fatalf("wrong plan: %+v %v", p, err)
	}
	out := filepath.Join(dir, "bundle")
	if err := Export(context.Background(), p, repo, out, "full"); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	b, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	json.Unmarshal(b, &manifest)
	hash := sha256.Sum256(content)
	if manifest.Files["Code.java"] != hex.EncodeToString(hash[:]) {
		t.Fatal("export checksum mismatch")
	}
	if _, err := os.Stat(filepath.Join(out, "source/.git")); !os.IsNotExist(err) {
		t.Fatal("git history exposed")
	}
	if Export(context.Background(), p, repo, out, "full") == nil {
		t.Fatal("existing bundle overwritten")
	}
	failed := filepath.Join(dir, "failed")
	invalid := p
	invalid.Commit = "missing"
	if Export(context.Background(), invalid, repo, failed, "full") == nil {
		t.Fatal("invalid revision exported")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatal("failed bundle not cleaned")
	}
	writeTicket([]any{version("0.9.0.1")})
	if _, err := Resolve(context.Background(), raw, repo, "", "", "1"); err == nil {
		t.Fatal("future release accepted")
	}
	writeTicket([]any{version("0.9.0.0"), version("0.9.0.1")})
	if _, err := Resolve(context.Background(), raw, repo, "", "", "1"); err == nil {
		t.Fatal("ambiguous affected versions accepted")
	}
}
