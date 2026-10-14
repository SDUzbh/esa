package esa

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestWeightedTokenAlerts(t *testing.T) {
	budget := Budget{MaxInputTokens: 16000, MaxOutputTokens: 6000, WarnTotalTokens: 22000}
	for _, tc := range []struct {
		name  string
		usage Usage
		codes string
	}{
		{"cache hits reduce input below threshold", Usage{InputTokens: 100000, CachedInputTokens: 95000, OutputTokens: 2000, TokensReported: true}, ""},
		{"uncached input retains full weight", Usage{InputTokens: 100000, OutputTokens: 2000, TokensReported: true}, "token_input_high,token_total_high"},
		{"exact weighted boundary", Usage{InputTokens: 97000, CachedInputTokens: 90000, OutputTokens: 6000, TokensReported: true}, ""},
		{"output and total", Usage{InputTokens: 97000, CachedInputTokens: 90000, OutputTokens: 6001, TokensReported: true}, "token_output_high,token_total_high"},
		{"unknown usage", Usage{InputTokens: 100000}, "token_usage_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var codes []string
			for _, alert := range TokenAlerts(tc.usage, budget) {
				codes = append(codes, alert.Code)
			}
			if strings.Join(codes, ",") != tc.codes {
				t.Fatalf("got %v, want %s", codes, tc.codes)
			}
		})
	}
	alerts := TokenAlerts(Usage{InputTokens: 189262, CachedInputTokens: 145920, OutputTokens: 2997, TokensReported: true}, budget)
	if len(alerts) != 2 || !strings.Contains(alerts[0].Message, "57934.00") || !strings.Contains(alerts[1].Message, "60931.00") {
		t.Fatal("ticket 7 weighted calculation is incorrect", alerts)
	}
	budget.TokenWeights = &TokenWeights{Input: 2, CachedInput: 0, Output: 3}
	alerts = TokenAlerts(Usage{InputTokens: 200000, CachedInputTokens: 200000, OutputTokens: 3000, TokensReported: true}, budget)
	if len(alerts) != 1 || alerts[0].Code != "token_output_high" {
		t.Fatal("explicit weights, including zero, were not respected", alerts)
	}
}

func TestTokenWeightsValidatedAndAggregateAlertsRefreshed(t *testing.T) {
	var budget Budget
	if err := json.Unmarshal([]byte(`{"token_weights":{"cached_input":0}}`), &budget); err != nil || budget.Weights() != (TokenWeights{Input: 1, CachedInput: 0, Output: 1}) {
		t.Fatal("partial weight config lost defaults or explicit zero", budget, err)
	}
	for _, weight := range []float64{-1, math.NaN(), math.Inf(1)} {
		config := testEngine(t).Config
		config.Budget.TokenWeights = &TokenWeights{Input: 1, CachedInput: weight, Output: 1}
		store, _ := OpenStore("")
		if _, err := NewEngine(store, config); err == nil {
			t.Fatal("invalid weight accepted", weight)
		}
	}
	report := Report{Usage: Usage{InputTokens: 100, CachedInputTokens: 50, TokensReported: true}, Alerts: []Alert{
		{Code: "token_input_high", Message: "earlier stage"}, {Code: "cleanup_failed", Message: "preserve cleanup warning"},
	}}
	UpdateTokenAlerts(&report, Budget{MaxInputTokens: 10})
	if len(report.Alerts) != 2 || report.Alerts[0].Code != "cleanup_failed" || !strings.Contains(report.Alerts[1].Message, "55.00") {
		t.Fatal("stale stage usage retained or other warning lost", report.Alerts)
	}
}
