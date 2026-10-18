package systemreplay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func history(t *testing.T) (Git, string, string) {
	t.Helper()
	root := t.TempDir()
	n := 0
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		stamp := time.Date(2020, 1, n+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	write := func(file, text string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(root, file), []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
	}
	commit := func(message string) { n++; git("add", "."); git("commit", "-m", message) }
	write("old.java", "a\nbug();\nz\n")
	commit("base")
	base := git("rev-parse", "HEAD")
	git("tag", "1.0")
	write("readme", "unrelated\n")
	commit("Documentation (#10)")
	git("tag", "1.1")
	write("old.java", "insert\na\nbug();\nz\n")
	commit("Move declarations (#11)")
	git("tag", "1.2")
	git("mv", "old.java", "new.java")
	commit("Rename (#12)")
	write("new.java", "insert\na\nfixed();\nz\n")
	commit("BUG-1: Correct completion\n\nCloses #19 from user/fix")
	fix := git("rev-parse", "HEAD")
	git("tag", "1.3")
	write("new.java", "insert\na\nbug();\nz\n")
	commit("Revert BUG-1 (#20)")
	git("tag", "1.4")
	git("checkout", "-b", "unmerged", base)
	write("old.java", "unmerged repair\n")
	commit("BUG-1 (#99)")
	git("checkout", "main")
	return Git{root, context.Background()}, base, fix
}
func TestMainlineScopeVersionsRenameAndUnmerged(t *testing.T) {
	g, base, fix := history(t)
	issue := Issue{ID: "issue_BUG-1", TicketKeys: []string{"BUG-1"}, Findings: []Finding{{Commit: base, File: "old.java", StartLine: 2, EndLine: 2}}}
	trace, e := g.Trace(issue, base, "main", "2020-01-02T00:00:01Z", "", 3)
	if e != nil {
		t.Fatal(e)
	}
	if len(trace.Versions) != 3 || len(trace.Candidates) != 3 {
		t.Fatalf("unexpected graph: %+v", trace)
	}
	found := false
	for _, c := range trace.Candidates {
		if strings.Contains(c.Message, "#99") || strings.Contains(c.Message, "Revert") {
			t.Fatal("unmerged or out-of-window commit included")
		}
		if c.Commit == fix {
			found = true
			if len(c.PRs) != 1 || c.PRs[0] != "#19" {
				t.Fatal(c.PRs)
			}
			if !strings.Contains(strings.Join(c.Reasons, " "), "Issue lines changed: new.java") {
				t.Fatal(c.Reasons)
			}
		}
	}
	if !found {
		t.Fatal("repair after rename was missed")
	}
	later, e := g.Trace(issue, base, "main", "2020-01-04T00:00:01Z", "", 2)
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range later.Candidates {
		if strings.Contains(c.Message, "Move declarations") {
			t.Fatal("pre-receipt change treated as future repair")
		}
	}
	if _, e = g.Trace(issue, fix, "main", "2020-01-02T00:00:01Z", "", 3); e == nil {
		t.Fatal("future baseline accepted")
	}
}
func TestTicketReferenceInMergedBranchLandsAtMainlineMerge(t *testing.T) {
	g, base, _ := history(t)
	git := func(date string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", g.Repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("2020-01-08T00:00:00Z", "checkout", "-b", "landing", base)
	if e := os.WriteFile(filepath.Join(g.Repo, "other.java"), []byte("repair();\n"), 0600); e != nil {
		t.Fatal(e)
	}
	git("2020-01-08T00:00:00Z", "add", ".")
	git("2020-01-08T00:00:00Z", "commit", "-m", "BUG-2: Repair other path (#55)")
	git("2020-01-09T00:00:00Z", "checkout", "main")
	git("2020-01-09T00:00:00Z", "merge", "--no-ff", "landing", "-m", "Merge implementation branch")
	landing := git("2020-01-09T00:00:00Z", "rev-parse", "HEAD")
	git("2020-01-09T00:00:00Z", "tag", "1.5")
	trace, e := g.Trace(Issue{ID: "BUG-2", TicketKeys: []string{"BUG-2"}}, base, "main", "2020-01-02T00:00:01Z", "", 5)
	if e != nil || len(trace.Candidates) != 1 || trace.Candidates[0].Commit != landing {
		t.Fatalf("%+v %v", trace, e)
	}
	if len(trace.Candidates[0].PRs) != 1 || trace.Candidates[0].PRs[0] != "#55" {
		t.Fatal(trace.Candidates[0])
	}
}

func TestUnknownFindingCommitKeepsTheTrace(t *testing.T) {
	g, base, fix := history(t)
	issue := Issue{TicketKeys: []string{"BUG-1"}, Findings: []Finding{{Commit: "fixture", File: "Example.java", StartLine: 1, EndLine: 1}}}
	trace, e := g.Trace(issue, base, "main", "2020-01-02T00:00:01Z", "", 3)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(strings.Join(trace.Limitations, " "), `"fixture" is not in the repository`) {
		t.Fatal(trace.Limitations)
	}
	for _, c := range trace.Candidates {
		if c.Commit == fix && strings.Contains(strings.Join(c.Reasons, " "), "Ticket reference: BUG-1") {
			return
		}
	}
	t.Fatalf("referenced fix lost: %+v", trace.Candidates)
}

func TestFailedExportLeavesNoPartialSnapshot(t *testing.T) {
	g, _, _ := history(t)
	run := func(args ...string) string {
		b, e := exec.Command("git", append([]string{"-C", g.Repo}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s", args, b)
		}
		return strings.TrimSpace(string(b))
	}
	if e := os.Symlink("new.java", filepath.Join(g.Repo, "link")); e != nil {
		t.Fatal(e)
	}
	run("add", "link")
	run("commit", "-q", "-m", "link")
	commit := run("rev-parse", "HEAD")
	root := t.TempDir()
	for range 2 {
		if _, _, e := export(t.Context(), g.Repo, commit, root); e == nil || !strings.Contains(e.Error(), "unsafe source archive entry: link") {
			t.Fatalf("export: %v", e)
		}
		if _, e := os.Stat(filepath.Join(root, "snapshots", commit)); !os.IsNotExist(e) {
			t.Fatalf("partial snapshot kept: %v", e)
		}
	}
	run("rm", "-q", "link")
	run("commit", "-q", "-m", "unlink")
	clean := run("rev-parse", "HEAD")
	for range 2 {
		if _, hashes, e := export(t.Context(), g.Repo, clean, root); e != nil || len(hashes) == 0 {
			t.Fatalf("clean export: %v %v", hashes, e)
		}
	}
}
