package esa

import (
	"fmt"
	"math"
)

func (b Budget) Weights() TokenWeights {
	if b.TokenWeights != nil {
		return *b.TokenWeights
	}
	return TokenWeights{Input: 1, CachedInput: 0.1, Output: 1}
}

func (b Budget) validWeights() bool {
	w := b.Weights()
	for _, weight := range []float64{w.Input, w.CachedInput, w.Output} {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight < 0 {
			return false
		}
	}
	return true
}

// Token thresholds are advisory. They never invalidate a useful diagnosis.
func TokenAlerts(u Usage, b Budget) []Alert {
	if !u.TokensReported {
		return []Alert{{Code: "token_usage_unknown", Message: "Provider token usage is unavailable; consumption is unknown, not zero."}}
	}
	var alerts []Alert
	w := b.Weights()
	// Older providers do not report cache hits; treat all input as uncached.
	cached := min(max(u.CachedInputTokens, 0), u.InputTokens)
	input := float64(u.InputTokens-cached)*w.Input + float64(cached)*w.CachedInput
	output := float64(u.OutputTokens) * w.Output
	for _, c := range []struct {
		name      string
		actual    float64
		threshold int
	}{
		{"input", input, b.MaxInputTokens}, {"output", output, b.MaxOutputTokens},
		{"total", input + output, b.WarnTotalTokens},
	} {
		if c.threshold > 0 && c.actual > float64(c.threshold) {
			alerts = append(alerts, Alert{Code: "token_" + c.name + "_high", Message: fmt.Sprintf("Token usage warning: weighted %s usage %.2f exceeds advisory threshold %d (raw input=%d, cached input=%d, output=%d; weights: input=%g, cached input=%g, output=%g). Analysis continues.", c.name, c.actual, c.threshold, u.InputTokens, cached, u.OutputTokens, w.Input, w.CachedInput, w.Output)})
		}
	}
	return alerts
}

func AddAlerts(report *Report, alerts ...Alert) {
	for _, alert := range alerts {
		found := false
		for _, old := range report.Alerts {
			if old.Code == alert.Code {
				found = true
				break
			}
		}
		if !found {
			report.Alerts = append(report.Alerts, alert)
		}
	}
}

// Refresh aggregate warnings after each stage, rather than retaining the first
// stage's lower counts under a deduplicated alert code. Stage audits are intact.
func UpdateTokenAlerts(report *Report, budget Budget) {
	kept := report.Alerts[:0]
	for _, alert := range report.Alerts {
		switch alert.Code {
		case "token_input_high", "token_output_high", "token_total_high", "token_usage_unknown":
		default:
			kept = append(kept, alert)
		}
	}
	report.Alerts = kept
	AddAlerts(report, TokenAlerts(report.Usage, budget)...)
}
