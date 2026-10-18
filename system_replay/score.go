package systemreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
)

// Labels are the evaluation set's expected outcomes, written by
// scripts/build_eval_manifest.py next to the manifest.
type Labels struct {
	Seed int                 `json:"seed"`
	Jobs map[string]JobLabel `json:"jobs"`
	Pool map[string]int      `json:"pool,omitempty"`
	Meta map[string]any      `json:"meta,omitempty"`
}

// JobLabel is one ticket's expected Routing decision: analyze for a fixed
// bug, reuse for a duplicate replayed after its original, return_to_support
// for a non-engineering request.
type JobLabel struct {
	ExpectedRoute string `json:"expected_route"`
	Original      string `json:"original,omitempty"`
	Stratum       string `json:"stratum,omitempty"`
	// Selected says why a bug is in the set: stratified sampling, or as the
	// original of a sampled duplicate.
	Selected string `json:"selected,omitempty"`
}

// Rate is a proportion with its 95% Wilson interval.
type Rate struct {
	Hits  int     `json:"hits"`
	N     int     `json:"n"`
	Value float64 `json:"value"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
}

// Mean is a sample mean with a 95% bootstrap interval.
type Mean struct {
	N     int     `json:"n"`
	Value float64 `json:"value"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
}

// Spread summarizes a cost measure across tickets.
type Spread struct {
	N      int     `json:"n"`
	Total  float64 `json:"total"`
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
}

// TicketScore is one ticket's observed outcome against its label.
type TicketScore struct {
	ID            string   `json:"id"`
	ExpectedRoute string   `json:"expected_route"`
	ObservedRoute string   `json:"observed_route"`
	RouteCorrect  bool     `json:"route_correct"`
	State         string   `json:"state"`
	GroundTruth   bool     `json:"ground_truth"`
	FixCommits    []string `json:"fix_commits,omitempty"`
	FixFiles      []string `json:"fix_files,omitempty"`
	Findings      int      `json:"findings"`
	FileHit       bool     `json:"file_hit"`
	LineHit       bool     `json:"line_hit"`
	Precision     float64  `json:"precision"`
	Recall        float64  `json:"recall"`
	InputTokens   float64  `json:"input_tokens"`
	CachedTokens  float64  `json:"cached_input_tokens"`
	OutputTokens  float64  `json:"output_tokens"`
	Calls         float64  `json:"calls"`
	ElapsedMS     int64    `json:"elapsed_ms"`
	Error         string   `json:"error,omitempty"`
}

// Score is the evaluation report.
type Score struct {
	Tickets      []TicketScore             `json:"tickets"`
	Routing      Rate                      `json:"routing_accuracy"`
	RoutingBy    map[string]Rate           `json:"routing_accuracy_by_expected"`
	Confusion    map[string]map[string]int `json:"routing_confusion"`
	Localized    int                       `json:"localization_tickets"`
	NoTruth      int                       `json:"analyze_tickets_without_ground_truth"`
	FileHit      Rate                      `json:"file_hit"`
	LineHit      Rate                      `json:"line_hit"`
	Precision    Mean                      `json:"precision"`
	Recall       Mean                      `json:"recall"`
	InputTokens  Spread                    `json:"input_tokens"`
	CachedTokens Spread                    `json:"cached_input_tokens"`
	OutputTokens Spread                    `json:"output_tokens"`
	Calls        Spread                    `json:"calls"`
	ElapsedMS    Spread                    `json:"elapsed_ms"`
	Unlabeled    []string                  `json:"unlabeled,omitempty"`
}

// testFile reports whether a changed file is a test, which is not counted as
// part of a fix's location.
func testFile(name string) bool {
	base := path.Base(name)
	inTests := slices.ContainsFunc(strings.Split(path.Dir(name), "/"), func(dir string) bool { return dir == "test" || dir == "tests" })
	return inTests || strings.HasSuffix(base, "Test.java") ||
		strings.HasSuffix(base, "Tests.java") || strings.HasPrefix(base, "test_")
}

// ScoreRun scores a finished run against its labels. repo is the product
// checkout the manifest used; it maps each finding from the commit the Agent
// read onto the parent of each fix commit, as the runner does.
func ScoreRun(ctx context.Context, summary Summary, labels Labels, repo string) Score {
	g := Git{repo, ctx}
	out := Score{RoutingBy: map[string]Rate{}, Confusion: map[string]map[string]int{}}
	var precision, recall []float64
	seen := map[string]bool{}
	for _, r := range summary.Results {
		label, ok := labels.Jobs[r.ID]
		if !ok {
			out.Unlabeled = append(out.Unlabeled, r.ID)
			continue
		}
		seen[r.ID] = true
		obs := initialObservation(r)
		t := TicketScore{ID: r.ID, ExpectedRoute: label.ExpectedRoute, ObservedRoute: obs.Route, State: obs.State, Findings: len(obs.Findings), Error: r.Error}
		if t.ObservedRoute == "" {
			t.ObservedRoute = "none"
		}
		t.RouteCorrect = t.ObservedRoute == label.ExpectedRoute
		if len(r.Events) > 0 {
			t.ElapsedMS = r.Events[0].ElapsedMS
		}
		t.InputTokens, t.CachedTokens = number(obs.Usage["input_tokens"]), number(obs.Usage["cached_input_tokens"])
		t.OutputTokens, t.Calls = number(obs.Usage["output_tokens"]), number(obs.Usage["calls"])
		if label.ExpectedRoute == "analyze" {
			scoreLocation(g, r, obs, &t)
			if t.GroundTruth {
				out.Localized++
				precision, recall = append(precision, t.Precision), append(recall, t.Recall)
			} else {
				out.NoTruth++
			}
		}
		if out.Confusion[t.ExpectedRoute] == nil {
			out.Confusion[t.ExpectedRoute] = map[string]int{}
		}
		out.Confusion[t.ExpectedRoute][t.ObservedRoute]++
		out.Tickets = append(out.Tickets, t)
	}
	sort.Slice(out.Tickets, func(i, j int) bool { return out.Tickets[i].ID < out.Tickets[j].ID })
	var routed, fileHits, lineHits int
	byExpected := map[string][2]int{}
	var input, cached, output, calls, elapsed []float64
	for _, t := range out.Tickets {
		b := byExpected[t.ExpectedRoute]
		b[1]++
		if t.RouteCorrect {
			routed++
			b[0]++
		}
		byExpected[t.ExpectedRoute] = b
		if t.GroundTruth && t.FileHit {
			fileHits++
		}
		if t.GroundTruth && t.LineHit {
			lineHits++
		}
		input, cached, output = append(input, t.InputTokens), append(cached, t.CachedTokens), append(output, t.OutputTokens)
		calls, elapsed = append(calls, t.Calls), append(elapsed, float64(t.ElapsedMS))
	}
	out.Routing = wilson(routed, len(out.Tickets))
	for route, b := range byExpected {
		out.RoutingBy[route] = wilson(b[0], b[1])
	}
	out.FileHit, out.LineHit = wilson(fileHits, out.Localized), wilson(lineHits, out.Localized)
	out.Precision, out.Recall = bootstrap(precision, labels.Seed), bootstrap(recall, labels.Seed+1)
	out.InputTokens, out.CachedTokens, out.OutputTokens = spread(input), spread(cached), spread(output)
	out.Calls, out.ElapsedMS = spread(calls), spread(elapsed)
	sort.Strings(out.Unlabeled)
	return out
}

// scoreLocation compares the findings with the fix: the commits the trace
// found by an exact ticket reference, never the ones it found because they
// touch a file the Agent named, so the truth does not depend on the
// prediction.
func scoreLocation(g Git, r Result, obs Observation, t *TicketScore) {
	if r.Trace == nil {
		return
	}
	type hunk struct {
		file         string
		start, count int
	}
	var hunks []hunk
	files := map[string]bool{}
	type fix struct {
		parent string
		hunks  []hunk
	}
	var fixes []fix
	for _, c := range r.Trace.Candidates {
		referenced := slices.ContainsFunc(c.Reasons, func(reason string) bool { return strings.HasPrefix(reason, "Ticket reference: ") })
		if !referenced {
			continue
		}
		f := fix{parent: c.Parent}
		for _, change := range c.Changes {
			if change.OldFile == "/dev/null" || testFile(change.OldFile) {
				continue
			}
			h := hunk{change.OldFile, change.OldStart, change.OldCount}
			f.hunks, hunks = append(f.hunks, h), append(hunks, h)
			files[change.OldFile] = true
		}
		if len(f.hunks) > 0 {
			fixes = append(fixes, f)
			t.FixCommits = append(t.FixCommits, c.Commit)
		}
	}
	if len(fixes) == 0 {
		return
	}
	t.GroundTruth = true
	for f := range files {
		t.FixFiles = append(t.FixFiles, f)
	}
	sort.Strings(t.FixFiles)
	covered := map[string]bool{}
	fileHits := 0
	for _, finding := range obs.Findings {
		fileHit, lineHit := false, false
		for _, f := range fixes {
			mapped := finding
			if finding.Commit != "" && finding.Commit != f.parent {
				if patch, e := g.run("diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--unified=0", finding.Commit, f.parent); e == nil {
					mapped = mapFindings([]Finding{finding}, changes(patch))[0]
				}
			}
			for _, h := range f.hunks {
				if h.file != mapped.File {
					continue
				}
				fileHit = true
				covered[h.file] = true
				if overlap(mapped.StartLine, mapped.EndLine, h.start, h.count) {
					lineHit = true
				}
			}
		}
		if fileHit {
			fileHits++
			t.FileHit = true
		}
		if lineHit {
			t.LineHit = true
		}
	}
	if len(obs.Findings) > 0 {
		t.Precision = float64(fileHits) / float64(len(obs.Findings))
	}
	t.Recall = float64(len(covered)) / float64(len(files))
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case int:
		return float64(n)
	}
	return 0
}

// wilson is the 95% Wilson score interval for hits out of n.
func wilson(hits, n int) Rate {
	r := Rate{Hits: hits, N: n}
	if n == 0 {
		return r
	}
	const z = 1.959963984540054
	p := float64(hits) / float64(n)
	nf := float64(n)
	center := (p + z*z/(2*nf)) / (1 + z*z/nf)
	half := z / (1 + z*z/nf) * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	r.Value, r.Low, r.High = p, math.Max(0, center-half), math.Min(1, center+half)
	return r
}

// bootstrap is the mean with a 95% percentile bootstrap interval from a
// seeded generator, so a report is reproducible.
func bootstrap(values []float64, seed int) Mean {
	m := Mean{N: len(values)}
	if len(values) == 0 {
		return m
	}
	mean := func(xs []float64) float64 {
		s := 0.0
		for _, x := range xs {
			s += x
		}
		return s / float64(len(xs))
	}
	m.Value = mean(values)
	rng := rand.New(rand.NewSource(int64(seed)))
	means := make([]float64, 2000)
	sample := make([]float64, len(values))
	for i := range means {
		for j := range sample {
			sample[j] = values[rng.Intn(len(values))]
		}
		means[i] = mean(sample)
	}
	sort.Float64s(means)
	m.Low, m.High = means[int(0.025*float64(len(means)))], means[int(0.975*float64(len(means)))-1]
	return m
}

func spread(values []float64) Spread {
	s := Spread{N: len(values)}
	if len(values) == 0 {
		return s
	}
	sorted := slices.Clone(values)
	sort.Float64s(sorted)
	for _, v := range sorted {
		s.Total += v
	}
	s.Mean = s.Total / float64(len(sorted))
	s.Median = sorted[len(sorted)/2]
	s.P90 = sorted[int(math.Ceil(0.9*float64(len(sorted))))-1]
	return s
}

// WriteScore writes report.json and a readable report.md into dir, which
// must be new.
func WriteScore(dir string, s Score) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(path.Join(dir, "report.json"), raw, 0o600); err != nil {
		return err
	}
	pct := func(r Rate) string {
		if r.N == 0 {
			return "n/a (0 tickets)"
		}
		return fmt.Sprintf("%.1f%% (%d/%d; 95%% CI %.1f–%.1f%%)", 100*r.Value, r.Hits, r.N, 100*r.Low, 100*r.High)
	}
	mean := func(m Mean) string {
		if m.N == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.2f (95%% CI %.2f–%.2f, n=%d)", m.Value, m.Low, m.High, m.N)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Agent evaluation\n\n")
	fmt.Fprintf(&b, "| Measure | Result |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| Routing accuracy | %s |\n", pct(s.Routing))
	routes := make([]string, 0, len(s.RoutingBy))
	for r := range s.RoutingBy {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		fmt.Fprintf(&b, "| Routing accuracy, expected %s | %s |\n", r, pct(s.RoutingBy[r]))
	}
	fmt.Fprintf(&b, "| Fix file found | %s |\n", pct(s.FileHit))
	fmt.Fprintf(&b, "| Fix lines overlapped | %s |\n", pct(s.LineHit))
	fmt.Fprintf(&b, "| Finding precision (files) | %s |\n", mean(s.Precision))
	fmt.Fprintf(&b, "| Fix-file recall | %s |\n", mean(s.Recall))
	fmt.Fprintf(&b, "| Input tokens per ticket | mean %.0f, median %.0f, p90 %.0f (total %.0f) |\n",
		s.InputTokens.Mean, s.InputTokens.Median, s.InputTokens.P90, s.InputTokens.Total)
	fmt.Fprintf(&b, "| Cached input tokens per ticket | mean %.0f, median %.0f, p90 %.0f |\n", s.CachedTokens.Mean, s.CachedTokens.Median, s.CachedTokens.P90)
	fmt.Fprintf(&b, "| Output tokens per ticket | mean %.0f, median %.0f, p90 %.0f (total %.0f) |\n",
		s.OutputTokens.Mean, s.OutputTokens.Median, s.OutputTokens.P90, s.OutputTokens.Total)
	fmt.Fprintf(&b, "| Wall time per ticket | mean %.1f s, median %.1f s, p90 %.1f s |\n\n", s.ElapsedMS.Mean/1000, s.ElapsedMS.Median/1000, s.ElapsedMS.P90/1000)
	fmt.Fprintf(&b, "Localization counts only expected-analyze tickets whose fix was found by an exact ticket reference (%d); %d had none. "+
		"A ticket without findings counts as a miss. Test files are not part of a fix.\n\n", s.Localized, s.NoTruth)
	fmt.Fprintf(&b, "## Routing confusion (expected → observed)\n\n| Expected | Observed | Tickets |\n| --- | --- | --- |\n")
	for _, e := range routes {
		observed := make([]string, 0)
		for o := range s.Confusion[e] {
			observed = append(observed, o)
		}
		sort.Strings(observed)
		for _, o := range observed {
			fmt.Fprintf(&b, "| %s | %s | %d |\n", e, o, s.Confusion[e][o])
		}
	}
	fmt.Fprintf(&b, "\n## Tickets\n\n| Ticket | Expected | Observed | Findings | File hit | Line hit | Precision | Recall | Tokens in/out | Seconds |\n"+
		"| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, t := range s.Tickets {
		truth := func(v bool) string {
			if !t.GroundTruth {
				return "–"
			}
			if v {
				return "yes"
			}
			return "no"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s | %.2f | %.2f | %.0f/%.0f | %.1f |\n", t.ID, t.ExpectedRoute, t.ObservedRoute, t.Findings,
			truth(t.FileHit), truth(t.LineHit), t.Precision, t.Recall, t.InputTokens, t.OutputTokens, float64(t.ElapsedMS)/1000)
	}
	return os.WriteFile(path.Join(dir, "report.md"), []byte(b.String()), 0o600)
}
