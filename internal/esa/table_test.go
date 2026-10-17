package esa

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
)

type tableValue struct {
	N    int
	Text string
	Tags []string
}

// checkTable compares t with the reference map m in every way the API offers.
func checkTable(t *testing.T, tb Table[tableValue], m map[string]tableValue, keys []string) {
	t.Helper()
	if tb.Len() != len(m) {
		t.Fatalf("Len %d, want %d", tb.Len(), len(m))
	}
	for _, k := range keys {
		got, ok := tb.Lookup(k)
		want, wantOK := m[k]
		if ok != wantOK || ok && (got.N != want.N || got.Text != want.Text) {
			t.Fatalf("Lookup(%q) = %v %v, want %v %v", k, got, ok, want, wantOK)
		}
	}
	seen := map[string]tableValue{}
	for k, v := range tb.All() {
		if _, dup := seen[k]; dup {
			t.Fatalf("All yielded %q twice", k)
		}
		seen[k] = v
	}
	if len(seen) != len(m) {
		t.Fatalf("All yielded %d entries, want %d", len(seen), len(m))
	}
}

func randomTableOps(t *testing.T, keySpace, steps int, seed uint64) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed+1))
	keys := make([]string, keySpace)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
	}
	var tb Table[tableValue]
	m := map[string]tableValue{}
	type version struct {
		tb Table[tableValue]
		m  map[string]tableValue
	}
	var versions []version
	for step := 0; step < steps; step++ {
		k := keys[r.IntN(len(keys))]
		before := tb
		var beforeMap = maps.Clone(m)
		if r.IntN(3) == 0 {
			tb.Delete(k)
			delete(m, k)
		} else {
			v := tableValue{N: step, Text: k}
			tb.Put(k, v)
			m[k] = v
		}
		if step%97 == 0 {
			versions = append(versions, version{before, beforeMap})
		}
		// diffTable against the previous version reports exactly the change.
		c := diffTable(before, tb)
		var wantUp, wantDel []string
		if _, ok := m[k]; ok {
			wantUp = []string{k}
		} else if _, was := beforeMap[k]; was {
			wantDel = []string{k}
		}
		if !slices.Equal(c.Upserted, wantUp) || !slices.Equal(c.Deleted, wantDel) {
			t.Fatalf("step %d: diff %v, want upserted %v deleted %v", step, c, wantUp, wantDel)
		}
	}
	checkTable(t, tb, m, keys)
	// Older versions are untouched by later writes.
	for _, v := range versions {
		checkTable(t, v.tb, v.m, keys)
	}
}

func TestTableMatchesMap(t *testing.T) {
	randomTableOps(t, 3000, 20000, 1)
	randomTableOps(t, 40, 5000, 2) // dense: many deletes, collapses and re-inserts
}

func TestTableHashCollisions(t *testing.T) {
	saved := tableHash
	defer func() { tableHash = saved }()
	// Every key shares the same hash: the trie bottoms out in a collision node.
	tableHash = func(string) uint64 { return 42 }
	randomTableOps(t, 30, 2000, 3)
	// Only a few distinct hashes: deep chains mixed with collision nodes.
	tableHash = func(k string) uint64 { return uint64(len(k)%3) << 60 }
	randomTableOps(t, 200, 5000, 4)
}

func TestTableDiffAfterManyChanges(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	base := map[string]tableValue{}
	for i := 0; i < 5000; i++ {
		base[fmt.Sprintf("k%d", i)] = tableValue{N: i}
	}
	old := TableOf(base)
	next := old
	want := map[string]bool{}
	deleted := map[string]bool{}
	for i := 0; i < 300; i++ {
		k := fmt.Sprintf("k%d", r.IntN(6000))
		switch r.IntN(3) {
		case 0:
			next.Delete(k)
		case 1:
			next.Put(k, tableValue{N: -i})
		default: // same value again: not a change
			if v, ok := next.Lookup(k); ok {
				next.Put(k, v)
			}
		}
	}
	for k := range base {
		if _, ok := next.Lookup(k); !ok {
			deleted[k] = true
		} else if next.Get(k).N != base[k].N {
			want[k] = true
		}
	}
	for k, v := range next.All() {
		if _, ok := base[k]; !ok && v.N <= 0 {
			want[k] = true
		}
	}
	c := diffTable(old, next)
	if len(c.Upserted) != len(want) || len(c.Deleted) != len(deleted) {
		t.Fatalf("diff found %d upserts / %d deletes, want %d / %d", len(c.Upserted), len(c.Deleted), len(want), len(deleted))
	}
	for _, k := range c.Upserted {
		if !want[k] {
			t.Fatalf("unexpected upsert %q", k)
		}
	}
	for _, k := range c.Deleted {
		if !deleted[k] {
			t.Fatalf("unexpected delete %q", k)
		}
	}
	if c := diffTable(old, old); len(c.Upserted)+len(c.Deleted) != 0 {
		t.Fatal("a table differs from itself")
	}
}

func TestTableJSONMatchesMapEncoding(t *testing.T) {
	m := map[string]tableValue{"b": {N: 2, Tags: []string{"x"}}, "a": {N: 1}, "<&>": {Text: "html   chars"}, "": {}}
	want, _ := json.Marshal(m)
	got, err := json.Marshal(TableOf(m))
	if err != nil || string(got) != string(want) {
		t.Fatalf("table JSON %s, map JSON %s (%v)", got, want, err)
	}
	var back Table[tableValue]
	if err = json.Unmarshal(got, &back); err != nil || back.Len() != len(m) || back.Get("b").Tags[0] != "x" {
		t.Fatal("round trip failed", err)
	}
	empty, _ := json.Marshal(Table[tableValue]{})
	emptyMap, _ := json.Marshal(map[string]tableValue{})
	if string(empty) != string(emptyMap) {
		t.Fatalf("empty table %s, empty map %s", empty, emptyMap)
	}
	var fromNull Table[tableValue]
	if err = json.Unmarshal([]byte("null"), &fromNull); err != nil || fromNull.Len() != 0 {
		t.Fatal("null did not decode to an empty table", err)
	}
	type holder struct {
		T Table[tableValue] `json:"t,omitzero"`
	}
	if b, _ := json.Marshal(holder{}); string(b) != "{}" {
		t.Fatalf("empty table not omitted: %s", b)
	}
}
