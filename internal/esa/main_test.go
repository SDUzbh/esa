package esa

import (
	"flag"
	"os"
	"testing"
)

// TestMain turns on the published-state guard for every test in this package.
// Benchmarks run without it so they measure the store, not the guard.
func TestMain(m *testing.M) {
	flag.Parse()
	guardPublishedState = flag.Lookup("test.bench").Value.String() == ""
	os.Exit(m.Run())
}
