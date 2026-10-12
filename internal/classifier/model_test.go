package classifier

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func samples(t *testing.T, name string) []Example {
	t.Helper()
	f, err := os.Open("../../examples/classifier/" + name + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	es, err := ReadExamples(f)
	if err != nil {
		t.Fatal(err)
	}
	return es
}
func fitted(t *testing.T) *Model {
	t.Helper()
	m, err := Train(context.Background(), samples(t, "train"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTFIDFAnalytic(t *testing.T) {
	es := []Example{
		{ID: "a", ProjectID: "1", Text: "apple apple pear", Labels: []string{"fruit"}, Reviewer: "human", ReviewedAt: time.Unix(1, 0)},
		{ID: "b", ProjectID: "1", Text: "pear carrot", Labels: []string{}, Reviewer: "human", ReviewedAt: time.Unix(1, 0)},
	}
	m, err := Train(context.Background(), es, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.IDF[m.index["apple"]]-(1+math.Log(3.0/2))) > 1e-12 || m.IDF[m.index["pear"]] != 1 {
		t.Fatal("smoothed idf incorrect")
	}
	x := m.vector("apple apple pear")
	norm := 0.0
	values := map[int]float64{}
	for _, f := range x {
		norm += f.value * f.value
		values[f.index] = f.value
	}
	if math.Abs(norm-1) > 1e-12 {
		t.Fatal("L2 normalization incorrect")
	}
	want := (1 + math.Log(2)) * (1 + math.Log(3.0/2))
	got := values[m.index["apple"]] / values[m.index["pear"]]
	if math.Abs(got-want) > 1e-12 {
		t.Fatal("sublinear tf incorrect", got, want)
	}
}

func TestMultilabelRoundTripDeterminismAndMetrics(t *testing.T) {
	m := fitted(t)
	es := samples(t, "train")
	for i, j := 0, len(es)-1; i < j; i, j = i+1, j-1 {
		es[i], es[j] = es[j], es[i]
	}
	again, err := Train(context.Background(), es, DefaultOptions())
	if err != nil || again.Version != m.Version {
		t.Fatal("nondeterministic fit", err)
	}
	r, err := m.Evaluate(samples(t, "test"))
	if err != nil {
		t.Fatal(err)
	}
	if r.MicroF1 < 0.95 || r.SubsetAccuracy != 1 {
		t.Fatalf("synthetic separable cases not learned: %+v", r)
	}
	p := m.Predict("consumer crash null pointer slow latency throughput regression")
	if !reflect.DeepEqual(p.Labels, []string{"bug", "performance"}) {
		t.Fatal(p)
	}
	if p := m.Predict("zzxyqv"); p.Status != "no_known_features" || len(p.Labels) != 0 {
		t.Fatal("out-of-vocabulary input was not withheld", p)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if err = m.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Predict("crash slow latency"), loaded.Predict("crash slow latency")) {
		t.Fatal("round trip changed predictions")
	}
	if err = m.Save(path); err == nil {
		t.Fatal("candidate overwrote existing file")
	}
	loaded.Models[0].Weights[0] += 1
	if err = loaded.Validate(); err == nil {
		t.Fatal("corrupt fingerprint accepted")
	}
}

func TestEvaluationLeakageAndUnknownLabels(t *testing.T) {
	m := fitted(t)
	valid := samples(t, "test")[0]
	for _, mutation := range []func(*Example){
		func(e *Example) { e.GroupID = "train-1" },
		func(e *Example) { e.Text = samples(t, "train")[0].Text },
		func(e *Example) { e.ReviewedAt = m.TrainingThrough },
		func(e *Example) { e.Labels = []string{"unknown"} },
		func(e *Example) { e.ProjectID = "2" },
	} {
		e := valid
		mutation(&e)
		if _, err := m.Evaluate([]Example{e}); err == nil {
			t.Fatal("invalid evaluation accepted", e)
		}
	}
}

func TestHumanFeedbackCutoffAndReplacement(t *testing.T) {
	base := samples(t, "train")
	correction := base[0]
	correction.Labels = []string{"usage"}
	correction.Reviewer = "reviewer-2"
	correction.ReviewedAt = correction.ReviewedAt.Add(time.Hour)
	future := correction
	future.Labels = []string{"performance"}
	future.ReviewedAt = future.ReviewedAt.Add(time.Hour)
	es, err := MergeFeedback(base, []Example{correction, future}, correction.ReviewedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != len(base) {
		t.Fatal("feedback was duplicated")
	}
	found := false
	for _, e := range es {
		if e.ID == correction.ID {
			found = true
			if !reflect.DeepEqual(e.Labels, correction.Labels) {
				t.Fatal("future correction included or human correction ignored")
			}
		}
	}
	if !found {
		t.Fatal("correction missing")
	}
	m, err := Train(context.Background(), es, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if m.Version == fitted(t).Version {
		t.Fatal("feedback did not change model")
	}
	before := fitted(t).Predict(correction.Text)
	after := m.Predict(correction.Text)
	for i, score := range after.Scores {
		if score.Label == "usage" && score.Score <= before.Scores[i].Score {
			t.Fatal("corrected human label did not gain score")
		}
		if score.Label == "bug" && score.Score >= before.Scores[i].Score {
			t.Fatal("removed human label did not lose score")
		}
	}
	conflict := correction
	conflict.Labels = []string{"performance"}
	if _, err := MergeFeedback(base, []Example{correction, conflict}, correction.ReviewedAt); err == nil {
		t.Fatal("ambiguous same-time correction accepted")
	}
}

func TestInvalidTrainingAndStrictData(t *testing.T) {
	es := samples(t, "train")
	for i := range es {
		es[i].Labels = []string{"constant"}
	}
	if _, err := Train(context.Background(), es, DefaultOptions()); err == nil {
		t.Fatal("constant label accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Train(ctx, samples(t, "train"), DefaultOptions()); err == nil {
		t.Fatal("cancelled fit proceeded")
	}
	for _, raw := range []string{`{"id":"x","future_resolution":"answer"}`, `{"id":"x"} {}`} {
		if _, err := ReadExamples(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid data accepted")
		}
	}
	if sigmoid(1000) != 1 || sigmoid(-1000) != 0 {
		t.Fatal("unstable sigmoid")
	}
}
