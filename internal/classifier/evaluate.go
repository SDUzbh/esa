package classifier

import (
	"fmt"
)

type LabelMetrics struct {
	Label     string  `json:"label"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	Support   int     `json:"support"`
	TP        int     `json:"true_positive"`
	FP        int     `json:"false_positive"`
	FN        int     `json:"false_negative"`
}
type Metrics struct {
	ModelVersion   string         `json:"model_version"`
	Count          int            `json:"count"`
	MicroPrecision float64        `json:"micro_precision"`
	MicroRecall    float64        `json:"micro_recall"`
	MicroF1        float64        `json:"micro_f1"`
	MacroF1        float64        `json:"macro_f1"`
	SubsetAccuracy float64        `json:"subset_accuracy"`
	HammingLoss    float64        `json:"hamming_loss"`
	Abstentions    int            `json:"abstentions"`
	Labels         []LabelMetrics `json:"labels"`
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
func f1(p, r float64) float64 {
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// Evaluate never refits the vocabulary or threshold on the held-out data.
func (m *Model) Evaluate(es []Example) (Metrics, error) {
	r := Metrics{ModelVersion: m.Version, Count: len(es), Labels: []LabelMetrics{}}
	if len(es) == 0 {
		return r, fmt.Errorf("empty test dataset")
	}
	groups := map[string]bool{}
	texts := map[string]bool{}
	labels := map[string]int{}
	ids := map[string]bool{}
	for _, g := range m.TrainingGroups {
		groups[g] = true
	}
	for _, t := range m.TrainingTexts {
		texts[t] = true
	}
	for j, l := range m.Models {
		labels[l.Label] = j
		r.Labels = append(r.Labels, LabelMetrics{Label: l.Label})
	}
	exact := 0
	for _, e := range es {
		if err := e.Validate(); err != nil {
			return r, err
		}
		if e.ProjectID != m.ProjectID || ids[e.ID] {
			return r, fmt.Errorf("mixed projects or duplicate test id")
		}
		ids[e.ID] = true
		if groups[GroupKey(e)] || texts[TextHash(e.Text)] {
			return r, fmt.Errorf("training/test overlap for %s", e.ID)
		}
		if !e.ReviewedAt.After(m.TrainingThrough) {
			return r, fmt.Errorf("test review for %s must be after training cutoff", e.ID)
		}
		truth := map[string]bool{}
		for _, l := range e.Labels {
			if _, ok := labels[l]; !ok {
				return r, fmt.Errorf("unknown test label %s", l)
			}
			truth[l] = true
		}
		p := m.Predict(e.Text)
		if p.Status != "suggested" {
			r.Abstentions++
		}
		pred := map[string]bool{}
		for _, l := range p.Labels {
			pred[l] = true
		}
		match := true
		for j, l := range r.Labels {
			if truth[l.Label] {
				r.Labels[j].Support++
			}
			switch {
			case truth[l.Label] && pred[l.Label]:
				r.Labels[j].TP++
			case pred[l.Label]:
				r.Labels[j].FP++
				match = false
			case truth[l.Label]:
				r.Labels[j].FN++
				match = false
			}
		}
		if match {
			exact++
		}
	}
	tp, fp, fn := 0, 0, 0
	for j, l := range r.Labels {
		l.Precision = ratio(l.TP, l.TP+l.FP)
		l.Recall = ratio(l.TP, l.TP+l.FN)
		l.F1 = f1(l.Precision, l.Recall)
		r.Labels[j] = l
		r.MacroF1 += l.F1
		tp += l.TP
		fp += l.FP
		fn += l.FN
	}
	r.MacroF1 /= float64(len(r.Labels))
	r.MicroPrecision = ratio(tp, tp+fp)
	r.MicroRecall = ratio(tp, tp+fn)
	r.MicroF1 = f1(r.MicroPrecision, r.MicroRecall)
	r.SubsetAccuracy = ratio(exact, len(es))
	r.HammingLoss = ratio(fp+fn, len(es)*len(r.Labels))
	return r, nil
}
