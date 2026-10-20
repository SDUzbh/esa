package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"esa/internal/esa"
	"esa/internal/retrieval"
)

// The offline engine must rank exactly as ESA does for the same records, or
// the measurement says nothing about ESA: the shipped variant against an
// exported Issue directory, the old baseline against retrieval's zero Options.
func TestEngineMatchesESA(t *testing.T) {
	base := time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)
	summaries := []string{"deadlock heartbeat GroupMetadata", "consumer rebalance hangs", "NullPointerException in JsonConverter",
		"deadlock in log cleaner", "producer timeout core", "heartbeat expiry", "excluded control deadlock"}
	var corpus []record
	var exported []esa.IssueRecord
	cutoff := base.Add(24 * time.Hour)
	for i, summary := range summaries {
		r := record{Key: fmt.Sprintf("KAFKA-%d", 100+i), Created: base.Add(time.Duration(i) * time.Hour), Summary: summary,
			Description: "Seen on 0.10.1.0 " + summary, Components: []string{"core"}, Versions: []string{"0.10.1.0"}}
		if i%2 == 0 {
			r.Resolved = r.Created.Add(time.Minute)
		}
		if i == 3 {
			r.Environment = "RHEL7, Java 8"
		}
		corpus = append(corpus, r)
		if i == len(summaries)-1 {
			continue // a control's own record is never in its directory
		}
		environment := map[string]string{}
		if r.Environment != "" {
			environment["environment"] = r.Environment
		}
		exported = append(exported, esa.IssueRecord{ID: r.Key, IssueID: r.Key, ProjectID: "1", ScopeHash: "scope", Kind: "engineer_resolved",
			Title: r.Summary, SourceKey: r.Key, Summary: r.Summary, Description: r.Description, Environment: environment,
			Components: r.Components, Versions: r.Versions, Recommendations: []string{"fix"}, AvailableAt: available(r, cutoff)})
	}
	root := t.TempDir()
	relative, digest, err := esa.ExportIssueDirectory(root, "1", cutoff, exported)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := esa.OpenIssueDirectory(root, relative, digest)
	if err != nil {
		t.Fatal(err)
	}
	var shipped, old *prepared
	for _, v := range variants {
		switch v.Name {
		case "esa_issue_search_2+5":
			shipped = prepare(v, corpus)
		case "baseline":
			old = prepare(v, corpus)
		}
	}
	var documents []retrieval.Document
	for _, r := range corpus[:len(corpus)-1] {
		documents = append(documents, retrieval.Document{ID: r.Key, Text: searchText(r)})
	}
	oldIndex := retrieval.Build(documents)
	queries := []string{"", "deadlock", "deadlocks heartbeat", "heartbeat GroupMetadata", "group metadata", "core 0.10.1.0", "rhel7", "nothing matches"}
	for _, query := range queries {
		rows, total, err := directory.Search(query, "", 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		hits := shipped.search(query, len(corpus), len(corpus)-1, cutoff)
		if total != len(hits) {
			t.Fatalf("%q: directory matches %d, offline %d", query, total, len(hits))
		}
		for i, row := range rows {
			if row.IssueID != corpus[hits[i].doc].Key {
				t.Fatalf("%q: position %d is %s in the directory, %s offline", query, i+1, row.IssueID, corpus[hits[i].doc].Key)
			}
		}
		if query == "" {
			continue
		}
		want := oldIndex.Search(query)
		got := old.search(query, len(corpus), len(corpus)-1, cutoff)
		if len(want) != len(got) {
			t.Fatalf("%q: old search matches %d, offline %d", query, len(want), len(got))
		}
		for i := range want {
			if want[i].Score != got[i].score {
				t.Fatalf("%q: old score %v offline %v at %d", query, want[i].Score, got[i].score, i)
			}
		}
	}
}

func TestVariantTokenizers(t *testing.T) {
	if got := tokensNumbers("Kafka 0.10.1.0 at 16:22:44,639 lock 0x00007f8c9c954378 on port 9092 broker1"); !reflect.DeepEqual(got, []string{"0.10.1.0", "kafka", "at", "lock", "on", "port", "broker1"}) {
		t.Fatal(got)
	}
	if got := tokensStems("Deadlocks hanging GroupMetadataManager"); !reflect.DeepEqual(got, []string{"deadlock", "hang", "groupmetadatamanager", "group", "metadata", "manager"}) {
		t.Fatal(got)
	}
	if got := tokensCombined("Deadlocks on 0.10.1.0 at 16:22:44 in GroupMetadata 0x7f"); !reflect.DeepEqual(got, []string{"0.10.1.0", "deadlock", "on", "at", "in", "groupmetadata", "group", "metadata"}) {
		t.Fatal(got)
	}
	sig, ok := signatureOf("Crash", "java.lang.IllegalStateException: x\n\tat kafka.log.Log.append(Log.scala:10)\n\tat kafka.log.Log.append(Log.scala:11)\n\tat org.apache.kafka.common.Foo$Bar.run(Foo.java:3)", "")
	if !ok || sig.exception != "IllegalStateException" || !reflect.DeepEqual(sig.frames, []string{"Log.append", "Foo$Bar.run"}) {
		t.Fatal(sig, ok)
	}
}
