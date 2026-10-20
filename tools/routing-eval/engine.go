package main

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"esa/internal/retrieval"
)

// variant is one way to index and match directory records. The baseline is
// exactly ESA's retrieval package; each other variant changes one thing.
type variant struct {
	Name string
	// Tokens turns text into index terms; queries use the same function.
	Tokens func(string) []string
	// Fielded scores title, body and metadata separately, the title by
	// TitleWeight and the others by 1.
	Fielded     bool
	TitleWeight float64
	// MinShare drops records holding fewer than this share of the distinct
	// query terms; zero keeps ESA's any-word matching.
	MinShare float64
}

var baseTokens = regexp.MustCompile(`[a-z0-9_]+`)

// tokensBase is ESA's tokenizer before Issue 15 (retrieval's zero Options).
func tokensBase(text string) []string { return retrieval.Options{}.Terms(text) }

var dotted = regexp.MustCompile(`\d+(?:\.\d+)+`)

// tokensNumbers keeps dotted versions such as 0.10.1.0 as one term and drops
// bare numbers and hex addresses, which come from timestamps, ports and
// thread dumps and match nearly every record (measure 1).
func tokensNumbers(text string) []string {
	lower := strings.ToLower(text)
	terms := dotted.FindAllString(lower, -1)
	for _, t := range baseTokens.FindAllString(dotted.ReplaceAllString(lower, " "), -1) {
		if strings.Trim(t, "0123456789") == "" || strings.HasPrefix(t, "0x") && strings.Trim(t[2:], "0123456789abcdef") == "" {
			continue
		}
		terms = append(terms, t)
	}
	return terms
}

// tokensStems is measure 2, as ESA ships it: identifier parts and light
// stemming (retrieval.Options.Stem).
func tokensStems(text string) []string { return retrieval.Options{Stem: true}.Terms(text) }

// tokensCombined is measures 1 and 2 together: dotted versions stay whole,
// bare numbers and hex addresses are dropped, and words also yield their
// identifier parts, all lightly stemmed.
func tokensCombined(text string) []string {
	terms := dotted.FindAllString(strings.ToLower(text), -1)
	for _, t := range tokensStems(dotted.ReplaceAllString(text, " ")) {
		if strings.Trim(t, "0123456789") == "" || strings.HasPrefix(t, "0x") && strings.Trim(t[2:], "0123456789abcdef") == "" {
			continue
		}
		terms = append(terms, t)
	}
	return terms
}

// fields of a record for the fielded variant; the unfielded variants index
// searchText as one field, exactly as ESA does.
func fields(r record, fielded bool) []string {
	if !fielded {
		return []string{searchText(r)}
	}
	return []string{r.Summary, r.Description + " " + r.Environment, r.Key + " " + strings.Join(r.Components, " ") + " " + strings.Join(r.Versions, " ")}
}

// prepared is a variant's index over the whole corpus, sorted by creation.
// A case's directory is a prefix of it, so df and lengths are read per prefix.
type prepared struct {
	v        variant
	corpus   []record
	counts   [][]map[string]int // [field][doc]
	lengths  [][]int
	prefix   [][]int // [field][n] total length of docs [0,n)
	postings []map[string][]int
}

func prepare(v variant, corpus []record) *prepared {
	nf := 1
	if v.Fielded {
		nf = 3
	}
	p := &prepared{v: v, corpus: corpus, counts: make([][]map[string]int, nf), lengths: make([][]int, nf), prefix: make([][]int, nf), postings: make([]map[string][]int, nf)}
	for f := 0; f < nf; f++ {
		p.counts[f] = make([]map[string]int, len(corpus))
		p.lengths[f] = make([]int, len(corpus))
		p.prefix[f] = make([]int, len(corpus)+1)
		p.postings[f] = map[string][]int{}
	}
	for i, r := range corpus {
		for f, text := range fields(r, v.Fielded) {
			counts := map[string]int{}
			for _, t := range v.Tokens(text) {
				counts[t]++
				p.lengths[f][i]++
			}
			p.counts[f][i] = counts
			p.prefix[f][i+1] = p.prefix[f][i] + p.lengths[f][i]
			for t := range counts {
				p.postings[f][t] = append(p.postings[f][t], i)
			}
		}
	}
	return p
}

type hit struct {
	doc   int
	score float64
}

// search ranks the directory made of corpus[0:n] without corpus[excluded],
// as IssueDirectory.search orders one kind: score, then latest
// availability, then key. An empty query browses everything.
func (p *prepared) search(query string, n, excluded int, cutoff time.Time) []hit {
	docs := n
	if excluded >= 0 && excluded < n {
		docs--
	} else {
		excluded = -1
	}
	var hits []hit
	unique := map[string]bool{}
	for _, t := range p.v.Tokens(query) {
		unique[t] = true
	}
	ordered := make([]string, 0, len(unique))
	for t := range unique {
		ordered = append(ordered, t)
	}
	sort.Strings(ordered)
	if strings.TrimSpace(query) == "" {
		for i := 0; i < n; i++ {
			if i != excluded {
				hits = append(hits, hit{doc: i})
			}
		}
	} else if docs > 0 && len(ordered) > 0 {
		type fieldStats struct {
			avg float64
			idf map[string]float64
		}
		stats := make([]fieldStats, len(p.counts))
		candidates := map[int]bool{}
		for f := range p.counts {
			total := p.prefix[f][n]
			if excluded >= 0 {
				total -= p.lengths[f][excluded]
			}
			stats[f] = fieldStats{avg: float64(total) / float64(docs), idf: map[string]float64{}}
			for _, t := range ordered {
				list := p.postings[f][t]
				df := sort.SearchInts(list, n)
				if excluded >= 0 && p.counts[f][excluded][t] > 0 {
					df--
				}
				stats[f].idf[t] = math.Log(1 + (float64(docs-df)+0.5)/(float64(df)+0.5))
				for _, d := range list[:sort.SearchInts(list, n)] {
					if d != excluded {
						candidates[d] = true
					}
				}
			}
		}
		for d := range candidates {
			score, matched := 0.0, 0
			for _, t := range ordered {
				present := false
				for f := range p.counts {
					frequency := float64(p.counts[f][d][t])
					if frequency == 0 || stats[f].avg == 0 {
						continue
					}
					present = true
					weight := 1.0
					if p.v.Fielded && f == 0 {
						weight = p.v.TitleWeight
					}
					score += weight * stats[f].idf[t] * frequency * 2.2 / (frequency + 1.2*(0.25+0.75*float64(p.lengths[f][d])/stats[f].avg))
				}
				if present {
					matched++
				}
			}
			if score > 0 && float64(matched) >= p.v.MinShare*float64(len(ordered)) {
				hits = append(hits, hit{doc: d, score: score})
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		a, b := available(p.corpus[hits[i].doc], cutoff), available(p.corpus[hits[j].doc], cutoff)
		if !a.Equal(b) {
			return a.After(b)
		}
		return p.corpus[hits[i].doc].Key < p.corpus[hits[j].doc].Key
	})
	return hits
}
