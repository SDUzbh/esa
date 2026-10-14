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
)

type Document struct {
	ID   string
	Text string
}
type Hit struct {
	ID    string
	Score float64
}

var tokens = regexp.MustCompile(`[a-z0-9_]+`)

func terms(text string) []string { return tokens.FindAllString(strings.ToLower(text), -1) }

type Index struct {
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
func Build(documents []Document) *Index {
	docs := append([]Document(nil), documents...)
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	b, _ := json.Marshal(docs)
	sum := sha256.Sum256(b)
	corpus := hex.EncodeToString(sum[:])
	if len(docs) == 0 {
		return &Index{corpus: corpus}
	}
	counts := make([]map[string]int, len(docs))
	lengths := make([]int, len(docs))
	df := map[string]int{}
	total := 0
	for index, doc := range docs {
		counts[index] = map[string]int{}
		for _, term := range terms(doc.Text) {
			counts[index][term]++
			lengths[index]++
			total++
		}
		for term := range counts[index] {
			df[term]++
		}
	}
	if total == 0 {
		return &Index{corpus: corpus}
	}
	avg := float64(total) / float64(len(docs))
	return &Index{docs: docs, counts: counts, lengths: lengths, df: df, average: avg, corpus: corpus}
}
func (index *Index) Search(query string) []Hit {
	docs, counts, lengths, df, avg := index.docs, index.counts, index.lengths, index.df, index.average
	if len(docs) == 0 {
		return []Hit{}
	}
	queryTerms := map[string]bool{}
	for _, term := range terms(query) {
		queryTerms[term] = true
	}
	ordered := []string{}
	for term := range queryTerms {
		ordered = append(ordered, term)
	}
	sort.Strings(ordered)
	hits := []Hit{}
	for index, doc := range docs {
		score := 0.0
		for _, term := range ordered {
			frequency := float64(counts[index][term])
			if frequency == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(docs)-df[term])+0.5)/(float64(df[term])+0.5))
			score += idf * frequency * 2.2 / (frequency + 1.2*(0.25+0.75*float64(lengths[index])/avg))
		}
		if score > 0 {
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
