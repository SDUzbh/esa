package esa_test

import (
	"encoding/json"
	"esa/internal/esa"
	"os"
	"testing"
	"time"
)

// This tests temporal case visibility, not a complete historical replay runner.
func TestFutureKnowledgeExcludedAndWithdrawalVersioned(t *testing.T) {
	before := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	approved := before.Add(time.Hour)
	withdrawn := approved.Add(time.Hour)
	draft := esa.Case{ID: "case-1", TicketID: "old", ProjectID: "1", Category: "code_defect", State: "draft", Version: 1, CreatedAt: before, UpdatedAt: before, Components: []string{"consumer"}, Versions: []string{"3.7.0"}, Signals: []string{"nullpointerexception"}, Keywords: []string{"consumer", "partition", "assignment", "nullpointerexception"}, Environment: map[string]string{"jdk": "17"}, Resolution: esa.Resolution{Solution: "Future solution"}}
	trusted := draft
	trusted.State = "trusted"
	trusted.Version = 2
	trusted.TrustedAt = approved
	trusted.UpdatedAt = approved
	invalidated := trusted
	invalidated.State = "invalidated"
	invalidated.Version = 3
	invalidated.UpdatedAt = withdrawn
	history := []esa.Case{draft, trusted, invalidated}
	if len(esa.CasesAt(history, before.Add(time.Minute))) != 0 {
		t.Fatal("future approval leaked")
	}
	if cases := esa.CasesAt(history, approved); len(cases) != 1 || cases[0].Version != 2 {
		t.Fatal("historical trusted revision unavailable")
	}
	if len(esa.CasesAt(history, withdrawn)) != 0 {
		t.Fatal("withdrawn case still active")
	}
	b, err := os.ReadFile("../../config/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg esa.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	store, err := esa.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := esa.NewEngine(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ticket := esa.Ticket{ID: "new", ProjectID: "1", Subject: "NullPointerException", Description: "Consumer partition assignment", Components: []string{"consumer"}, Versions: []string{"3.7.0"}, Environment: map[string]string{"jdk": "17"}}
	ticket.ForwardedBy = "fixture-human"
	ticket.ForwardedAt = before
	_, route, _, _ := engine.Decide(ticket, history, before.Add(time.Minute))
	if route == "case_reuse" {
		t.Fatal("workflow used future solution")
	}
	_, route, _, _ = engine.Decide(ticket, history, approved)
	if route != "case_reuse" {
		t.Fatal("workflow did not use visible trusted case")
	}
}
