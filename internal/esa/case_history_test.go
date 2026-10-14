package esa_test

import (
	"esa/internal/esa"
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
}
