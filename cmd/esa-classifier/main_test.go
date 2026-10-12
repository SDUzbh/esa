package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"esa/internal/classifier"
)

func TestIndependentCommandsAndFeedbackRetraining(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	var out bytes.Buffer
	train := []string{"train", "-data", "../../examples/classifier/train.jsonl", "-output", path, "-cutoff", "2025-01-01T00:00:00Z"}
	if err := run(context.Background(), train, &out); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"test", "-model", path, "-data", "../../examples/classifier/test.jsonl"}, {"predict", "-model", path, "-text", "consumer crash null pointer slow latency regression"}} {
		out.Reset()
		if err := run(context.Background(), args, &out); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatal("invalid JSON result")
		}
	}
	f, err := os.Open("../../examples/classifier/train.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	es, err := classifier.ReadExamples(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	es[0].Labels = []string{"usage"}
	es[0].ReviewedAt = es[0].ReviewedAt.AddDate(0, 0, 1)
	b, _ := json.Marshal(es[0])
	feedback := filepath.Join(dir, "feedback.jsonl")
	if err = os.WriteFile(feedback, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	updated := filepath.Join(dir, "candidate.json")
	out.Reset()
	if err = run(context.Background(), []string{"retrain", "-data", "../../examples/classifier/train.jsonl", "-feedback", feedback, "-output", updated, "-cutoff", "2025-01-03T00:00:00Z"}, &out); err != nil {
		t.Fatal(err)
	}
	m, err := classifier.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := classifier.Load(updated)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version == candidate.Version {
		t.Fatal("candidate unchanged")
	}
	if err = run(context.Background(), train, &out); err == nil {
		t.Fatal("existing model overwritten")
	}
	if err = run(context.Background(), []string{"test", "-model", path, "-data", "../../examples/classifier/train.jsonl"}, &out); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatal("CLI allowed train/test leakage", err)
	}
}
