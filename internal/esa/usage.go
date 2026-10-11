package esa

import "fmt"

// Token thresholds are advisory. They never invalidate a useful diagnosis.
func TokenAlerts(u Usage, b Budget) []Alert {
	if !u.TokensReported {
		return []Alert{{Code: "token_usage_unknown", Message: "Provider token usage is unavailable; consumption is unknown, not zero."}}
	}
	var alerts []Alert
	for _, c := range []struct {
		name              string
		actual, threshold int
	}{
		{"input", u.InputTokens, b.MaxInputTokens}, {"output", u.OutputTokens, b.MaxOutputTokens},
		{"total", u.InputTokens + u.OutputTokens, b.WarnTotalTokens},
	} {
		if c.threshold > 0 && c.actual > c.threshold {
			alerts = append(alerts, Alert{Code: "token_" + c.name + "_high", Message: fmt.Sprintf("Token usage warning: %s usage %d exceeds advisory threshold %d. Analysis continues.", c.name, c.actual, c.threshold)})
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
