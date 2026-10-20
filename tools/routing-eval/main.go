// Command routing-eval measures the Routing Agent's Issue search offline at
// realistic directory sizes (review Issue 15). For every case it builds the
// directory a ticket created at that moment would see, from every Fixed
// Kafka ticket created earlier, and records where each query style ranks the
// true original, under ESA's current index and under each candidate change.
// It makes no model or service calls.
//
// Usage: routing-eval -corpus corpus.jsonl -cases cases.json -out results.json
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"esa/internal/esa"
)

// record is one Fixed ticket as an Issue directory record would describe it.
type record struct {
	Key         string    `json:"key"`
	Created     time.Time `json:"created"`
	Resolved    time.Time `json:"resolved"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"`
	Environment string    `json:"environment"`
	Components  []string  `json:"components"`
	Versions    []string  `json:"versions"`
}

type evalCase struct {
	Kind                string            `json:"kind"`
	Key                 string            `json:"key"`
	Created             time.Time         `json:"created"`
	Original            string            `json:"original,omitempty"`
	OriginalFixedBefore bool              `json:"original_fixed_before,omitempty"`
	Queries             map[string]string `json:"queries"`
	Summary             string            `json:"summary"`
	Description         string            `json:"description"`
	Environment         string            `json:"environment"`
}

// result is one query of one case.
type result struct {
	Style string `json:"style"`
	// Rank is the original's 1-based position, 0 when the query misses it.
	Rank    int     `json:"rank"`
	Matches int     `json:"matches"`
	Top     float64 `json:"top_score"`
	Score   float64 `json:"original_score,omitempty"`
}

type caseResult struct {
	Kind          string   `json:"kind"`
	Key           string   `json:"key"`
	Original      string   `json:"original,omitempty"`
	DirectorySize int      `json:"directory_size"`
	Results       []result `json:"results"`
}

// signatureResult is measure 4 for one case.
type signatureResult struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	// HasSignature: the incoming ticket quotes an exception and a product frame.
	HasSignature bool `json:"has_signature"`
	// Exact counts records with the same exception and top three frames;
	// Loose counts records sharing at least two of the top three frames.
	Exact         int  `json:"exact_matches"`
	ExactOriginal bool `json:"exact_original"`
	Loose         int  `json:"loose_matches"`
	LooseOriginal bool `json:"loose_original"`
	// BoostedRank is the title query's rank for the original when records
	// with an exact or loose signature match are listed first.
	BoostedRank int `json:"boosted_title_rank"`
}

// snippetResult is measure 7's proxy for one case: a word-overlap reader
// picks the most similar of the title query's top 10, seeing titles only or
// titles with a one-line snippet.
type snippetResult struct {
	Kind            string  `json:"kind"`
	Key             string  `json:"key"`
	OriginalInTop10 bool    `json:"original_in_top10"`
	PickedTitle     bool    `json:"picked_with_titles"`
	PickedSnippet   bool    `json:"picked_with_snippets"`
	BestTitle       float64 `json:"best_similarity_titles"`
	BestSnippet     float64 `json:"best_similarity_snippets"`
	OriginalTitle   float64 `json:"original_similarity_titles,omitempty"`
	OriginalSnippet float64 `json:"original_similarity_snippets,omitempty"`
	SnippetChars    int     `json:"snippet_chars"`
}

type output struct {
	Variants   map[string][]caseResult `json:"variants"`
	Signatures []signatureResult       `json:"signatures"`
	Snippets   []snippetResult         `json:"snippets"`
}

// searchText is the text the Issue directory indexes for a record:
// IssueIndexEntry's title, then SourceKey, Summary, Description and the
// environment as JSON, then components and versions
// (internal/esa/issue_directory.go). The ticket's summary stands in for both
// the Issue title and the diagnosis summary, which the export does not hold.
func searchText(r record) string {
	environment, _ := json.Marshal(map[string]string{"environment": r.Environment})
	if r.Environment == "" {
		environment, _ = json.Marshal(map[string]string{})
	}
	title := []rune(strings.TrimSpace(r.Summary))
	if len(title) > 1000 {
		title = title[:1000]
	}
	return string(title) + " " + r.Key + " " + r.Summary + " " + r.Description + " " + string(environment) + " " +
		strings.Join(r.Components, " ") + " " + strings.Join(r.Versions, " ")
}

// available is when the record's knowledge became visible to a ticket created
// at cutoff: its fix if that came first, otherwise its own creation (an open
// original is in the directory once analyzed).
func available(r record, cutoff time.Time) time.Time {
	if !r.Resolved.IsZero() && !r.Resolved.After(cutoff) {
		return r.Resolved
	}
	return r.Created
}

var (
	frame     = regexp.MustCompile(`\bat (?:org\.apache\.kafka|kafka)\.(?:[\w$]+\.)*?([A-Z][\w$]*)\.([\w$<>]+)\(`)
	exception = regexp.MustCompile(`\b([A-Z]\w*(?:Exception|Error))\b`)
)

func distinct(values []string, limit int) []string {
	var out []string
	for _, v := range values {
		seen := false
		for _, o := range out {
			seen = seen || o == v
		}
		if !seen {
			out = append(out, v)
		}
		if len(out) == limit {
			break
		}
	}
	return out
}

// frames returns the first distinct product frames as Class.method.
func frames(text string, limit int) []string {
	var all []string
	for _, m := range frame.FindAllStringSubmatch(text, -1) {
		all = append(all, m[1]+"."+m[2])
	}
	return distinct(all, limit)
}

func exceptions(text string, limit int) []string {
	var all []string
	for _, m := range exception.FindAllStringSubmatch(text, -1) {
		all = append(all, m[1])
	}
	return distinct(all, limit)
}

// autoQuery is measure 6: the query ESA could build from the incoming ticket
// itself, instead of relying on the Agent's wording: its title, exception
// names and the classes of its product stack frames.
func autoQuery(c evalCase) string {
	text := c.Description + "\n" + c.Environment
	parts := []string{c.Summary}
	parts = append(parts, exceptions(c.Summary+"\n"+text, 3)...)
	var classes []string
	for _, f := range frames(text, 10) {
		classes = append(classes, strings.SplitN(f, ".", 2)[0])
	}
	return strings.Join(append(parts, distinct(classes, 5)...), " ")
}

type signature struct {
	exception string
	frames    []string
}

func signatureOf(summary, description, environment string) (signature, bool) {
	text := description + "\n" + environment
	f := frames(text, 3)
	e := exceptions(summary+"\n"+text, 1)
	if len(f) == 0 || len(e) == 0 {
		return signature{}, false
	}
	return signature{exception: e[0], frames: f}, true
}

func (a signature) exact(b signature) bool {
	return a.exception == b.exception && strings.Join(a.frames, "|") == strings.Join(b.frames, "|")
}

func (a signature) shared(b signature) int {
	n := 0
	for _, x := range a.frames {
		for _, y := range b.frames {
			if x == y {
				n++
			}
		}
	}
	return n
}

var stopwords = map[string]bool{"the": true, "and": true, "for": true, "with": true, "when": true, "not": true, "from": true,
	"this": true, "that": true, "are": true, "was": true, "can": true, "should": true, "does": true, "after": true, "into": true}

func wordSet(text string) map[string]bool {
	set := map[string]bool{}
	for _, t := range tokensNumbers(text) {
		if len(t) >= 3 && !stopwords[t] {
			set[t] = true
		}
	}
	return set
}

func jaccard(a, b map[string]bool) float64 {
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// snippet is a one-line hit summary: the first non-empty description line,
// at most 200 characters. A real record would show its diagnosis summary.
func snippet(r record) string {
	for _, line := range strings.Split(r.Description, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if runes := []rune(line); len(runes) > 200 {
				line = string(runes[:200])
			}
			return line
		}
	}
	return ""
}

func evaluate(corpus []record, cases []evalCase, variants []variant) output {
	position := map[string]int{}
	for i, r := range corpus {
		position[r.Key] = i
	}
	prefix := func(c evalCase) (int, int) {
		n := sort.Search(len(corpus), func(i int) bool { return !corpus[i].Created.Before(c.Created) })
		excluded := -1
		if i, ok := position[c.Key]; ok && i < n {
			excluded = i
		}
		return n, excluded
	}
	out := output{Variants: map[string][]caseResult{}}
	var base *prepared
	for _, v := range variants {
		p := prepare(v, corpus)
		if base == nil {
			base = p
		}
		for _, c := range cases {
			n, excluded := prefix(c)
			cr := caseResult{Kind: c.Kind, Key: c.Key, Original: c.Original, DirectorySize: n}
			if excluded >= 0 {
				cr.DirectorySize--
			}
			queries := map[string]string{"auto": autoQuery(c)}
			for style, q := range c.Queries {
				queries[style] = q
			}
			styles := make([]string, 0, len(queries))
			for style := range queries {
				styles = append(styles, style)
			}
			sort.Strings(styles)
			for _, style := range styles {
				hits := p.search(queries[style], n, excluded, c.Created)
				r := result{Style: style, Matches: len(hits)}
				if len(hits) > 0 {
					r.Top = hits[0].score
				}
				for i, h := range hits {
					if corpus[h.doc].Key == c.Original {
						r.Rank, r.Score = i+1, h.score
						break
					}
				}
				cr.Results = append(cr.Results, r)
			}
			out.Variants[v.Name] = append(out.Variants[v.Name], cr)
		}
	}
	signatures := make([]signature, len(corpus))
	has := make([]bool, len(corpus))
	for i, r := range corpus {
		signatures[i], has[i] = signatureOf(r.Summary, r.Description, r.Environment)
	}
	for _, c := range cases {
		n, excluded := prefix(c)
		sr := signatureResult{Kind: c.Kind, Key: c.Key}
		sig, ok := signatureOf(c.Summary, c.Description, c.Environment)
		sr.HasSignature = ok
		matched := map[int]bool{}
		if ok {
			for i := 0; i < n; i++ {
				if i == excluded || !has[i] {
					continue
				}
				original := corpus[i].Key == c.Original
				if sig.exact(signatures[i]) {
					sr.Exact++
					sr.ExactOriginal = sr.ExactOriginal || original
					matched[i] = true
				}
				if sig.shared(signatures[i]) >= 2 {
					sr.Loose++
					sr.LooseOriginal = sr.LooseOriginal || original
					matched[i] = true
				}
			}
		}
		titleHits := base.search(c.Queries["title"], n, excluded, c.Created)
		hits := append([]hit(nil), titleHits...)
		sort.SliceStable(hits, func(i, j int) bool { return matched[hits[i].doc] && !matched[hits[j].doc] })
		for i, h := range hits {
			if corpus[h.doc].Key == c.Original {
				sr.BoostedRank = i + 1
				break
			}
		}
		out.Signatures = append(out.Signatures, sr)

		// Measure 7: the reader sees the title query's first page.
		top := titleHits
		if len(top) > 10 {
			top = top[:10]
		}
		incoming := wordSet(c.Summary + " " + firstChars(c.Description, 300))
		snip := snippetResult{Kind: c.Kind, Key: c.Key}
		pickTitle, pickSnippet := -1, -1
		for _, h := range top {
			r := corpus[h.doc]
			s := snippet(r)
			snip.SnippetChars += len(s)
			a, b := jaccard(incoming, wordSet(r.Summary)), jaccard(incoming, wordSet(r.Summary+" "+s))
			if r.Key == c.Original {
				snip.OriginalInTop10, snip.OriginalTitle, snip.OriginalSnippet = true, a, b
			}
			if a > snip.BestTitle || pickTitle < 0 {
				snip.BestTitle, pickTitle = a, h.doc
			}
			if b > snip.BestSnippet || pickSnippet < 0 {
				snip.BestSnippet, pickSnippet = b, h.doc
			}
		}
		snip.PickedTitle = pickTitle >= 0 && corpus[pickTitle].Key == c.Original
		snip.PickedSnippet = pickSnippet >= 0 && corpus[pickSnippet].Key == c.Original
		out.Snippets = append(out.Snippets, snip)
	}
	return out
}

func firstChars(s string, n int) string {
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n])
	}
	return s
}

func readCorpus(path string) ([]record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var corpus []record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var r record
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return nil, err
		}
		corpus = append(corpus, r)
	}
	sort.SliceStable(corpus, func(i, j int) bool { return corpus[i].Created.Before(corpus[j].Created) })
	return corpus, scanner.Err()
}

// variants are ESA's index before Issue 15, one change each (measures 1, 2,
// 3 and 5), combinations, and the search ESA now ships (2 + 5).
var variants = []variant{
	{Name: "baseline", Tokens: tokensBase},
	{Name: "1_numbers", Tokens: tokensNumbers},
	{Name: "2_identifiers_stems", Tokens: tokensStems},
	{Name: "3_fields_title_x3", Tokens: tokensBase, Fielded: true, TitleWeight: 3},
	{Name: "3_fields_title_x1.5", Tokens: tokensBase, Fielded: true, TitleWeight: 1.5},
	{Name: "5_half_the_words", Tokens: tokensBase, MinShare: 0.5},
	{Name: "5_third_of_the_words", Tokens: tokensBase, MinShare: 1.0 / 3},
	{Name: "combined_1+2", Tokens: tokensCombined},
	{Name: "esa_issue_search_2+5", Tokens: esa.IssueSearch.Terms, MinShare: esa.IssueSearch.MinShare},
	{Name: "combined_1+2+5", Tokens: tokensCombined, MinShare: 1.0 / 3},
}

func run() error {
	corpusPath := flag.String("corpus", "", "corpus.jsonl from scripts/build_routing_corpus.py")
	casesPath := flag.String("cases", "", "cases.json from scripts/build_routing_corpus.py")
	outPath := flag.String("out", "", "Results file to write")
	flag.Parse()
	if *corpusPath == "" || *casesPath == "" || *outPath == "" {
		return fmt.Errorf("-corpus, -cases and -out are required")
	}
	corpus, err := readCorpus(*corpusPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*casesPath)
	if err != nil {
		return err
	}
	var cases []evalCase
	if err = json.Unmarshal(raw, &cases); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(evaluate(corpus, cases, variants), "", " ")
	return os.WriteFile(*outPath, append(data, '\n'), 0600)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "routing-eval:", err)
		os.Exit(1)
	}
}
