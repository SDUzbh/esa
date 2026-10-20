// Package retrieval builds a small, deterministic eligible-corpus projection.
// Callers must apply time and access policy before supplying documents.
package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type Document struct {
	ID   string
	Text string
}
type Hit struct {
	ID    string
	Score float64
}

var (
	tokens = regexp.MustCompile(`[a-z0-9_]+`)
	words  = regexp.MustCompile(`[A-Za-z0-9_]+`)
)

// Options selects how text becomes terms and how many of a query's terms a
// document must hold. The zero value is plain BM25 over lowercase words,
// where one shared word is a match.
type Options struct {
	// Stem also indexes the parts of code identifiers (GroupMetadataManager
	// as group, metadata and manager) and strips common plural and verb
	// endings, so code names and prose meet.
	Stem bool
	// MinShare drops documents holding fewer than this share of the distinct
	// query terms. Zero keeps any-word matching.
	MinShare float64
}

// Terms turns text into index and query terms.
func (o Options) Terms(text string) []string {
	if !o.Stem {
		return tokens.FindAllString(strings.ToLower(text), -1)
	}
	var terms []string
	for _, w := range words.FindAllString(text, -1) {
		terms = append(terms, stem(strings.ToLower(w)))
		if parts := identifierParts(w); len(parts) > 1 {
			for _, part := range parts {
				terms = append(terms, stem(strings.ToLower(part)))
			}
		}
	}
	return terms
}

// identifierParts splits camelCase, acronyms and underscores:
// JSONConverter_v2 is JSON, Converter, v2.
func identifierParts(w string) []string {
	var parts []string
	runes := []rune(w)
	start := 0
	for i := 1; i <= len(runes); i++ {
		boundary := i == len(runes) || runes[i] == '_' ||
			unicode.IsUpper(runes[i]) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) ||
			unicode.IsUpper(runes[i]) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(runes[i-1])
		if boundary {
			if part := strings.Trim(string(runes[start:i]), "_"); part != "" {
				parts = append(parts, part)
			}
			start = i
		}
	}
	return parts
}

// stem is a light suffix stripper, not a full stemmer: deadlocks and
// deadlock, hanging and hang meet; it never needs to produce real words.
func stem(w string) string {
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		return w[:len(w)-2]
	case len(w) > 4 && (strings.HasSuffix(w, "ses") || strings.HasSuffix(w, "xes") || strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes")):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

type Index struct {
	options Options
	docs    []Document
	counts  []map[string]int
	lengths []int
	df      map[string]int
	average float64
	corpus  string
}

func BM25(query string, documents []Document) ([]Hit, string) {
	index := Build(documents)
	return index.Search(query), index.corpus
}

// Build compiles a deterministic lexical index and corpus fingerprint.
// Callers must exclude inaccessible or future records before constructing it.
func Build(documents []Document) *Index { return BuildWith(documents, Options{}) }

// BuildWith is Build with the given term and matching options.
func BuildWith(documents []Document, options Options) *Index {
	docs := append([]Document(nil), documents...)
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	b, _ := json.Marshal(docs)
	sum := sha256.Sum256(b)
	corpus := hex.EncodeToString(sum[:])
	if len(docs) == 0 {
		return &Index{options: options, corpus: corpus}
	}
	counts := make([]map[string]int, len(docs))
	lengths := make([]int, len(docs))
	df := map[string]int{}
	total := 0
	for index, doc := range docs {
		counts[index] = map[string]int{}
		for _, term := range options.Terms(doc.Text) {
			counts[index][term]++
			lengths[index]++
			total++
		}
		for term := range counts[index] {
			df[term]++
		}
	}
	if total == 0 {
		return &Index{options: options, corpus: corpus}
	}
	avg := float64(total) / float64(len(docs))
	return &Index{options: options, docs: docs, counts: counts, lengths: lengths, df: df, average: avg, corpus: corpus}
}

// Search ranks candidate IDs using BM25; scores do not certify a shared cause.
func (index *Index) Search(query string) []Hit {
	docs, counts, lengths, df, avg := index.docs, index.counts, index.lengths, index.df, index.average
	if len(docs) == 0 {
		return []Hit{}
	}
	queryTerms := map[string]bool{}
	for _, term := range index.options.Terms(query) {
		queryTerms[term] = true
	}
	ordered := []string{}
	for term := range queryTerms {
		ordered = append(ordered, term)
	}
	sort.Strings(ordered)
	hits := []Hit{}
	minimum := index.options.MinShare * float64(len(ordered))
	for index, doc := range docs {
		score, matched := 0.0, 0
		for _, term := range ordered {
			frequency := float64(counts[index][term])
			if frequency == 0 {
				continue
			}
			matched++
			idf := math.Log(1 + (float64(len(docs)-df[term])+0.5)/(float64(df[term])+0.5))
			score += idf * frequency * 2.2 / (frequency + 1.2*(0.25+0.75*float64(lengths[index])/avg))
		}
		if score > 0 && float64(matched) >= minimum {
			hits = append(hits, Hit{ID: doc.ID, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	return hits
}
