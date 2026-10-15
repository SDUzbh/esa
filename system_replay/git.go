package systemreplay

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Git struct {
	Repo    string
	Context context.Context
}

// Created only after public predictions freeze. Successful reads are shared
// across tickets; mutable refs therefore resolve once for this evaluation run.
type historyReader struct {
	Git
	reads map[string]string
}

func (g *historyReader) run(args ...string) (string, error) {
	key := strings.Join(args, "\x00")
	if value, ok := g.reads[key]; ok {
		return value, nil
	}
	value, err := g.Git.run(args...)
	if err == nil {
		g.reads[key] = value
	}
	return value, err
}

func (g *historyReader) resolve(ref string) (string, error) {
	return g.run("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

func (g Git) run(args ...string) (string, error) {
	cmd := exec.CommandContext(g.Context, "git", append([]string{"-C", g.Repo}, args...)...)
	b, e := cmd.Output()
	if e != nil {
		return "", fmt.Errorf("git %s: %w", args[0], e)
	}
	return strings.TrimRight(string(b), "\n"), nil
}
func (g Git) resolve(ref string) (string, error) {
	return g.run("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

var releaseTag = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){1,3}$`)
var hunk = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)
var prNumber = regexp.MustCompile(`(?:\(#|(?i:pull request|pull-request|PR|closes|close|merged)\s*#?)([0-9]+)`)
var prURL = regexp.MustCompile(`https?://[^\s)]+/pull/[0-9]+`)

func changes(patch string) []Change {
	out := []Change{}
	old, new := "", ""
	hadHunk := false
	renamed := false
	flush := func() {
		if renamed && !hadHunk {
			out = append(out, Change{OldFile: old, NewFile: new})
		}
	}
	for _, s := range strings.Split(patch, "\n") {
		if strings.HasPrefix(s, "diff --git ") {
			flush()
			old, new = "", ""
			hadHunk = false
			renamed = false
		}
		if strings.HasPrefix(s, "rename from ") {
			old = strings.TrimPrefix(s, "rename from ")
			renamed = true
		}
		if strings.HasPrefix(s, "rename to ") {
			new = strings.TrimPrefix(s, "rename to ")
		}
		if strings.HasPrefix(s, "--- ") {
			old = strings.TrimPrefix(strings.TrimPrefix(s, "--- "), "a/")
		}
		if strings.HasPrefix(s, "+++ ") {
			new = strings.TrimPrefix(strings.TrimPrefix(s, "+++ "), "b/")
		}
		if m := hunk.FindStringSubmatch(s); m != nil {
			nums := []int{0, 1, 0, 1}
			for i := 0; i < 4; i++ {
				if m[i+1] != "" {
					nums[i], _ = strconv.Atoi(m[i+1])
				}
			}
			out = append(out, Change{old, new, nums[0], nums[1], nums[2], nums[3]})
			hadHunk = true
		}
	}
	flush()
	return out
}
func pullRequests(message string) []string {
	set := map[string]bool{}
	for _, s := range prURL.FindAllString(message, -1) {
		set[s] = true
	}
	for _, m := range prNumber.FindAllStringSubmatch(message, -1) {
		set["#"+m[1]] = true
	}
	out := []string{}
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Trace follows first-parent mainline repair candidates after the initial observation.
// Merged branch references land at their mainline merge; unmerged work is excluded.
func (g Git) Trace(issue Issue, baseline, mainline, cutoff, until string, count int) (Trace, error) {
	return (&historyReader{g, map[string]string{}}).Trace(issue, baseline, mainline, cutoff, until, count)
}
func (g *historyReader) Trace(issue Issue, baseline, mainline, cutoff, until string, count int) (Trace, error) {
	out := Trace{Issue: issue, Candidates: []Candidate{}, Versions: []Version{}, State: "window_exhausted", Limitations: []string{"Scope matching triggers review; a file or line overlap alone does not establish a fix.", "PR evidence comes from local commit messages and patches. Missing PR discussion remains a review limitation.", "Release tags label observation points; all evaluated code is from the chosen main branch."}}
	if count < 1 {
		return out, fmt.Errorf("versions must be positive")
	}
	shallow, e := g.run("rev-parse", "--is-shallow-repository")
	if e != nil || shallow == "true" {
		return out, fmt.Errorf("complete Git history is required")
	}
	base, e := g.resolve(baseline)
	if e != nil {
		return out, e
	}
	head, e := g.resolve(mainline)
	if e != nil {
		return out, e
	}
	out.Baseline = base
	out.Mainline = mainline
	out.MainlineHead = head
	at, e := timestamp(cutoff)
	if e != nil {
		return out, e
	}
	end := time.Time{}
	if until != "" {
		end, e = timestamp(until)
		if e != nil || !end.After(at) {
			return out, fmt.Errorf("until must follow the ticket cutoff")
		}
	}
	stamp, e := g.run("show", "-s", "--format=%cI", base)
	if e != nil {
		return out, e
	}
	bt, e := timestamp(stamp)
	if e != nil || bt.After(at) {
		return out, fmt.Errorf("baseline is after ticket cutoff")
	}
	// Build the mainline graph once. A branch release is projected to the mainline
	// observation point at its publication commit time; it is not checked out.
	text, e := g.run("log", "--first-parent", "--reverse", "--format=%H %cI", head)
	if e != nil {
		return out, e
	}
	type node struct {
		id string
		at time.Time
	}
	chain := []node{}
	indices := map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		xs := strings.Fields(line)
		if len(xs) != 2 {
			continue
		}
		ts, e := timestamp(xs[1])
		if e != nil {
			return out, e
		}
		indices[xs[0]] = len(chain)
		chain = append(chain, node{xs[0], ts})
	}
	anchor := -1
	if i, ok := indices[base]; ok {
		anchor = i
	} else {
		mb, e := g.run("merge-base", base, head)
		if e != nil {
			return out, fmt.Errorf("baseline has no common mainline history")
		}
		var ok bool
		anchor, ok = indices[mb]
		if !ok {
			anchor = -1
		}
		for i, n := range chain {
			if !n.at.After(at) && i > anchor {
				anchor = i
			}
		}
		out.Limitations = append(out.Limitations, "Baseline is outside first-parent mainline; prediction lines are mapped to the mainline snapshot at ticket cutoff. Branch-only differences require reviewer interpretation.")
	}
	// Updates already present at receipt are not future repair events.
	for i, n := range chain {
		if i > anchor && !n.at.After(at) {
			anchor = i
		}
	}
	if anchor < 0 || anchor >= len(chain) {
		return out, fmt.Errorf("cannot locate baseline mainline anchor")
	}
	// Read tags in a batch instead of invoking Git separately for every version.
	refs, e := g.run("for-each-ref", "--format=%(refname:strip=2)%09%(objectname)%09%(*objectname)", "refs/tags")
	if e != nil {
		return out, e
	}
	tags := map[string]string{}
	for _, line := range strings.Split(refs, "\n") {
		xs := strings.Split(line, "\t")
		if len(xs) == 3 && releaseTag.MatchString(xs[0]) {
			id := xs[1]
			if xs[2] != "" {
				id = xs[2]
			}
			tags[xs[0]] = id
		}
	}
	ids := []string{}
	unique := map[string]bool{}
	for _, id := range tags {
		if !unique[id] {
			ids = append(ids, id)
			unique[id] = true
		}
	}
	sort.Strings(ids)
	dates := map[string]string{}
	// One Git log supplies tag commit dates, including branch-only release nodes.
	if len(ids) > 0 {
		args := append([]string{"show", "-s", "--format=%H %cI"}, ids...)
		r, e := g.run(args...)
		if e != nil {
			return out, e
		}
		for _, line := range strings.Split(r, "\n") {
			xs := strings.Fields(line)
			if len(xs) == 2 {
				dates[xs[0]] = xs[1]
			}
		}
	}
	for name, id := range tags {
		ts, e := timestamp(dates[id])
		if e != nil || !ts.After(at) || (!end.IsZero() && ts.After(end)) {
			continue
		}
		landing := -1
		if i, ok := indices[id]; ok {
			landing = i
		} else {
			for i, n := range chain {
				if !n.at.After(ts) {
					landing = i
				}
			}
		}
		if landing <= anchor {
			continue
		}
		out.Versions = append(out.Versions, Version{name, id, chain[landing].id, ts.Format(time.RFC3339)})
	}
	sort.Slice(out.Versions, func(i, j int) bool {
		a, b := out.Versions[i], out.Versions[j]
		ai, bi := indices[a.MainlineCommit], indices[b.MainlineCommit]
		if ai != bi {
			return ai < bi
		}
		if a.CommittedAt != b.CommittedAt {
			return a.CommittedAt < b.CommittedAt
		}
		return a.Name < b.Name
	})
	// Coalesce tags that observe the same mainline node. N counts code revisions.
	versions := []Version{}
	for _, v := range out.Versions {
		if len(versions) > 0 && versions[len(versions)-1].MainlineCommit == v.MainlineCommit {
			continue
		}
		versions = append(versions, v)
	}
	if len(versions) > count {
		versions = versions[:count]
	}
	out.Versions = versions
	if len(versions) == 0 {
		out.State = "no_future_version"
		return out, nil
	}
	stop := indices[versions[len(versions)-1].MainlineCommit]
	tracked := append([]Finding(nil), issue.Findings...)
	// Each linked ticket may have localized the same issue in a different
	// historical revision. Map every original finding into this mainline anchor.
	remaps := map[string][]Change{}
	for i, f := range tracked {
		original := f.Commit
		if original == "" {
			original = base
		}
		if original == chain[anchor].id {
			continue
		}
		delta, ok := remaps[original]
		if !ok {
			p, e := g.run("diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--unified=0", original, chain[anchor].id)
			if e != nil {
				return out, e
			}
			delta = changes(p)
			remaps[original] = delta
		}
		tracked[i] = mapFindings([]Finding{f}, delta)[0]
	}
	// Batch metadata and touched paths, then obtain patches only for issue-scope
	// commits. Most mainline changes cost no extra Git invocation.
	records, e := g.run("log", "--first-parent", "--reverse", "--diff-merges=first-parent", "--format=%x1e%H%x00%P%x00%B%x00", "--name-only", "--no-renames", chain[anchor].id+".."+chain[stop].id)
	if e != nil {
		return out, e
	}
	type metadata struct {
		parents string
		message string
		files   map[string]bool
	}
	meta := map[string]metadata{}
	for _, record := range strings.Split(records, "\x1e") {
		parts := strings.SplitN(record, "\x00", 4)
		if len(parts) != 4 {
			continue
		}
		m := metadata{parts[1], strings.TrimRight(parts[2], "\n"), map[string]bool{}}
		for _, file := range strings.Split(strings.TrimSpace(parts[3]), "\n") {
			m.files[file] = true
		}
		meta[strings.TrimSpace(parts[0])] = m
	}
	patterns := make([]string, len(issue.TicketKeys))
	keyPatterns := make([]*regexp.Regexp, len(issue.TicketKeys))
	for i, key := range issue.TicketKeys {
		patterns[i] = regexp.QuoteMeta(key)
		keyPatterns[i] = regexp.MustCompile(`\b` + patterns[i] + `\b`)
	}
	previous := chain[anchor].id
	for i := anchor + 1; i <= stop; i++ {
		id := chain[i].id
		m := meta[id]
		// Merge subjects need not mention the ticket. Include key-referencing
		// branch commits only when they actually land in this mainline step.
		if len(strings.Fields(m.parents)) > 1 && len(issue.TicketKeys) > 0 {
			related, e := g.run("log", "--format=%H%n%B", "--extended-regexp", "--grep", strings.Join(patterns, "|"), previous+".."+id)
			if e != nil {
				return out, e
			}
			if related != "" {
				m.message += "\n\nMerged issue-referencing Git history (parent..commit):\n" + related
			}
		}
		matched := false
		for _, pattern := range keyPatterns {
			if pattern.MatchString(m.message) {
				matched = true
			}
		}
		for _, f := range tracked {
			if m.files[f.File] {
				matched = true
			}
		}
		if !matched {
			previous = id
			continue
		}
		patch, e := g.run("diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--unified=0", previous, id)
		if e != nil {
			return out, e
		}
		delta := changes(patch)
		message := m.message
		reasons := []string{}
		for n, key := range issue.TicketKeys {
			if keyPatterns[n].MatchString(message) {
				reasons = append(reasons, "Ticket reference: "+key)
			}
		}
		for _, f := range tracked {
			for _, c := range delta {
				if c.OldFile == f.File {
					reason := "Issue file changed: " + f.File
					if overlap(f.StartLine, f.EndLine, c.OldStart, c.OldCount) {
						reason = "Issue lines changed: " + f.File
					}
					reasons = append(reasons, reason)
					break
				}
			}
		}
		if len(reasons) > 0 {
			version := ""
			for _, v := range versions {
				if indices[v.MainlineCommit] >= i {
					version = v.Name
					break
				}
			}
			subject := strings.Split(message, "\n")[0]
			out.Candidates = append(out.Candidates, Candidate{id, previous, version, subject, message, reasons, pullRequests(message), delta, patch})
		}
		tracked = mapFindings(tracked, delta)
		previous = id
	}
	if len(out.Candidates) > 0 {
		out.State = "awaiting_review"
	}
	return out, nil
}
func overlap(start, end, old, count int) bool {
	if count == 0 {
		return old >= start-1 && old <= end
	}
	return start <= old+count-1 && end >= old
}
func mapFindings(fs []Finding, cs []Change) []Finding {
	out := append([]Finding(nil), fs...)
	for i := range out {
		f := &out[i]
		file := f.File
		shift := 0
		start, end := f.StartLine, f.EndLine
		for _, c := range cs {
			if c.OldFile != file {
				continue
			}
			if c.NewFile != "/dev/null" {
				f.File = c.NewFile
			}
			if c.OldStart+c.OldCount <= start {
				shift += c.NewCount - c.OldCount
			} else if overlap(start, end, c.OldStart, c.OldCount) {
				end += c.NewCount - c.OldCount
			}
		}
		f.StartLine = start + shift
		f.EndLine = end + shift
		if f.StartLine < 1 {
			f.StartLine = 1
		}
		if f.EndLine < f.StartLine {
			f.EndLine = f.StartLine
		}
	}
	return out
}
