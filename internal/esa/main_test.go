package esa

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// TestMain turns on the published-state guard for every test in this package.
// Benchmarks run without it so they measure the store, not the guard. The
// package needs a test database; without one it stops before running anything.
func TestMain(m *testing.M) {
	flag.Parse()
	if os.Getenv("ESA_TEST_DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, missingTestDatabase)
		os.Exit(1)
	}
	guardPublishedState = flag.Lookup("test.bench").Value.String() == ""
	os.Exit(m.Run())
}
