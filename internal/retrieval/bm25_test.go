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

func TestStemmedTermsJoinIdentifiersAndProse(t *testing.T) {
	got := Options{Stem: true}.Terms("Deadlocks while hanging in GroupMetadataManager and JSONConverter_v2")
	want := []string{"deadlock", "while", "hang", "in", "groupmetadatamanager", "group", "metadata", "manager", "and",
		"jsonconverter_v2", "json", "converter", "v2"}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
	if plain := (Options{}).Terms("Deadlocks GroupMetadataManager"); len(plain) != 2 || plain[0] != "deadlocks" {
		t.Fatal("the zero options changed plain terms", plain)
	}
}

func TestMinimumShareOfQueryTerms(t *testing.T) {
	docs := []Document{
		{ID: "one-word", Text: "core heartbeat thread sends fetch requests"},
		{ID: "deadlock", Text: "GroupMetadataManager deadlocks with heartbeat expiry"},
	}
	query := "group metadata deadlock heartbeat"
	anyWord := BuildWith(docs, Options{Stem: true}).Search(query)
	if len(anyWord) != 2 {
		t.Fatal("any-word matching should return both", anyWord)
	}
	third := BuildWith(docs, Options{Stem: true, MinShare: 1.0 / 3}).Search(query)
	if len(third) != 1 || third[0].ID != "deadlock" {
		t.Fatal("a record holding one of four query terms passed the third-of-words rule", third)
	}
	if hits := BuildWith(docs, Options{}).Search("deadlock"); len(hits) != 0 {
		t.Fatal("plain terms matched a plural without stemming", hits)
	}
}
