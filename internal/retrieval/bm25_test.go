package retrieval

import "testing"

func TestDeterministicEligibleProjection(t *testing.T) {
	docs := []Document{{ID: "b", Text: "network timeout"}, {ID: "a", Text: "empty assignment assignment"}}
	hits, corpus := BM25("empty assignment", docs)
	if len(hits) != 1 || hits[0].ID != "a" {
		t.Fatal(hits)
	}
	reversed := []Document{docs[1], docs[0]}
	again, hash := BM25("empty assignment", reversed)
	if hash != corpus || again[0].Score != hits[0].Score {
		t.Fatal("input ordering changed scores or corpus identity")
	}
}
