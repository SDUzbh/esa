package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoReferencePreservesContractsWithoutBodies(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contract.go")
	source := `// Package sample documents a boundary.
package sample
// Box retains a generic value.
type Box[T any] struct {
    Value T // Original value.
}
// Read returns a value without changing the box.
func (b *Box[T]) Read(key string, rest ...int) (T, error) {
    panic("BODY_MUST_NOT_APPEAR")
}
// Closed is the terminal state.
const Closed = "closed"
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := render(&output, root, path); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@page go_", "Package sample", "Box[T any]", "*Box[T].Read", "rest ...int", "(T, error)", "Read returns", "Closed is", "contract.go:8"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("reference omits %q", want)
		}
	}
	if strings.Contains(output.String(), "BODY_MUST_NOT_APPEAR") {
		t.Fatal("function body leaked into declaration reference")
	}
	got, _ := os.ReadFile(path)
	if string(got) != source {
		t.Fatal("input source was changed")
	}
}

func TestFilterRejectsInvalidInputsAndSkipsTests(t *testing.T) {
	root := t.TempDir()
	var output bytes.Buffer
	if err := render(&output, root, filepath.Join(filepath.Dir(root), "outside.go")); err == nil {
		t.Fatal("accepted input outside root")
	}
	if err := render(&output, root, filepath.Join(root, "anything_test.go")); err != nil || output.Len() != 0 {
		t.Fatal("test files must be skipped")
	}
	broken := filepath.Join(root, "broken.go")
	if err := os.WriteFile(broken, []byte("package ???"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := render(&output, root, broken); err == nil {
		t.Fatal("silently accepted a parse failure")
	}
}
