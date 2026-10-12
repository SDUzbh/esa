package classifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Options struct {
	Labels       []string `json:"labels,omitempty"`
	MinDF        int      `json:"min_df"`
	MaxFeatures  int      `json:"max_features"`
	Epochs       int      `json:"epochs"`
	LearningRate float64  `json:"learning_rate"`
	L2           float64  `json:"l2"`
	Threshold    float64  `json:"threshold"`
}

func DefaultOptions() Options {
	return Options{MinDF: 1, MaxFeatures: 10000, Epochs: 300, LearningRate: 1, L2: 0.01, Threshold: 0.5}
}
func (o Options) validate() error {
	if o.MinDF < 1 || o.MaxFeatures < 1 || o.MaxFeatures > 50000 || o.Epochs < 1 || o.Epochs > 10000 || !finite(o.LearningRate) || o.LearningRate <= 0 || o.LearningRate > 1 || !finite(o.L2) || o.L2 < 0 || o.L2 > 1 || !finite(o.Threshold) || o.Threshold <= 0 || o.Threshold >= 1 {
		return fmt.Errorf("invalid training options")
	}
	return nil
}

type LabelModel struct {
	Label   string    `json:"label"`
	Weights []float64 `json:"weights"`
	Bias    float64   `json:"bias"`
}
type Model struct {
	SchemaVersion   int          `json:"schema_version"`
	Version         string       `json:"version"`
	ProjectID       string       `json:"project_id"`
	TrainingThrough time.Time    `json:"training_through"`
	TrainingCount   int          `json:"training_count"`
	DatasetHash     string       `json:"dataset_hash"`
	TrainingGroups  []string     `json:"training_groups"`
	TrainingTexts   []string     `json:"training_texts"`
	Options         Options      `json:"options"`
	Vocabulary      []string     `json:"vocabulary"`
	IDF             []float64    `json:"idf"`
	Models          []LabelModel `json:"models"`
	index           map[string]int
}
type feature struct {
	index int
	value float64
}
type Score struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}
type Prediction struct {
	ModelVersion string   `json:"model_version"`
	TextHash     string   `json:"text_hash"`
	Labels       []string `json:"labels"`
	Scores       []Score  `json:"scores"`
	Status       string   `json:"status"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func sigmoid(v float64) float64 {
	if v >= 0 {
		return 1 / (1 + math.Exp(-v))
	}
	z := math.Exp(v)
	return z / (1 + z)
}
func (m *Model) vector(text string) []feature {
	t := terms(text)
	x := []feature{}
	norm := 0.0
	for _, word := range sortedKeys(t) {
		if j, ok := m.index[word]; ok {
			v := (1 + math.Log(float64(t[word]))) * m.IDF[j]
			x = append(x, feature{j, v})
			norm += v * v
		}
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range x {
			x[i].value /= norm
		}
	}
	return x
}
func linear(l LabelModel, x []feature) float64 {
	z := l.Bias
	for _, f := range x {
		z += l.Weights[f.index] * f.value
	}
	return z
}
func (m *Model) Predict(text string) Prediction {
	p := Prediction{ModelVersion: m.Version, TextHash: TextHash(text), Labels: []string{}, Scores: []Score{}, Status: "suggested"}
	x := m.vector(text)
	if len(x) == 0 {
		p.Status = "no_known_features"
		return p
	}
	for _, l := range m.Models {
		s := sigmoid(linear(l, x))
		p.Scores = append(p.Scores, Score{l.Label, s})
		if s >= m.Options.Threshold {
			p.Labels = append(p.Labels, l.Label)
		}
	}
	return p
}
func (m *Model) Labels() []string {
	out := []string{}
	for _, l := range m.Models {
		out = append(out, l.Label)
	}
	return out
}
func (m *Model) contentVersion() string { copy := *m; copy.Version = ""; return digest(copy) }

func Train(ctx context.Context, examples []Example, o Options) (*Model, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if len(examples) < 2 {
		return nil, fmt.Errorf("at least two reviewed examples are required")
	}
	// Sort without modifying the caller's data; the dataset fingerprint is stable.
	es := append([]Example(nil), examples...)
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
	m := &Model{SchemaVersion: 1, ProjectID: es[0].ProjectID, Options: o, TrainingCount: len(es), DatasetHash: digest(es)}
	df := map[string]int{}
	labels := map[string]bool{}
	groups := map[string]bool{}
	texts := map[string]bool{}
	ids := map[string]bool{}
	for _, label := range o.Labels {
		if label == "" || label != strings.TrimSpace(label) || len(label) > 100 || labels[label] {
			return nil, fmt.Errorf("invalid label set")
		}
		labels[label] = true
	}
	for _, e := range es {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := e.Validate(); err != nil {
			return nil, err
		}
		if e.ProjectID != m.ProjectID || ids[e.ID] {
			return nil, fmt.Errorf("mixed projects or duplicate example id %s", e.ID)
		}
		ids[e.ID] = true
		if e.ReviewedAt.After(m.TrainingThrough) {
			m.TrainingThrough = e.ReviewedAt
		}
		groups[GroupKey(e)] = true
		texts[TextHash(e.Text)] = true
		for t := range terms(e.Text) {
			df[t]++
		}
		if len(df) > 500000 {
			return nil, fmt.Errorf("candidate vocabulary exceeds 500,000 terms; reduce or normalize corpus")
		}
		for _, l := range e.Labels {
			if len(o.Labels) > 0 && !labels[l] {
				return nil, fmt.Errorf("unknown label %s", l)
			}
			labels[l] = true
		}
	}
	if len(labels) == 0 || len(labels) > 64 || len(labels)*o.MaxFeatures > 2000000 {
		return nil, fmt.Errorf("label set is empty or exceeds model size limit")
	}
	m.TrainingGroups = sortedKeys(groups)
	m.TrainingTexts = sortedKeys(texts)
	var vocab []string
	for t, n := range df {
		if n >= o.MinDF {
			vocab = append(vocab, t)
		}
	}
	sort.Slice(vocab, func(i, j int) bool {
		if df[vocab[i]] != df[vocab[j]] {
			return df[vocab[i]] > df[vocab[j]]
		}
		return vocab[i] < vocab[j]
	})
	if len(vocab) > o.MaxFeatures {
		vocab = vocab[:o.MaxFeatures]
	}
	sort.Strings(vocab)
	if len(vocab) == 0 {
		return nil, fmt.Errorf("empty vocabulary; reduce min_df")
	}
	m.Vocabulary = vocab
	m.IDF = make([]float64, len(vocab))
	m.index = map[string]int{}
	for j, t := range vocab {
		m.index[t] = j
		m.IDF[j] = 1 + math.Log(float64(1+len(es))/float64(1+df[t]))
	}
	xs := make([][]feature, len(es))
	for i, e := range es {
		xs[i] = m.vector(e.Text)
	}
	for _, label := range sortedKeys(labels) {
		ys := make([]bool, len(es))
		positives := 0
		for i, e := range es {
			for _, l := range e.Labels {
				if l == label {
					ys[i] = true
					positives++
				}
			}
		}
		if positives == 0 || positives == len(es) {
			return nil, fmt.Errorf("label %s needs both positive and negative examples", label)
		}
		l := LabelModel{Label: label, Weights: make([]float64, len(vocab))}
		grad := make([]float64, len(vocab))
		for epoch := 0; epoch < o.Epochs; epoch++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for j, w := range l.Weights {
				grad[j] = o.L2 * w
			}
			gb := 0.0
			for i, x := range xs {
				y, weight := 0.0, 0.5/float64(len(es)-positives)
				if ys[i] {
					y = 1
					weight = 0.5 / float64(positives)
				}
				g := (sigmoid(linear(l, x)) - y) * weight
				gb += g
				for _, f := range x {
					grad[f.index] += g * f.value
				}
			}
			for j, g := range grad {
				l.Weights[j] -= o.LearningRate * g
			}
			l.Bias -= o.LearningRate * gb
		}
		m.Models = append(m.Models, l)
	}
	m.Version = m.contentVersion()
	return m, m.Validate()
}

func (m *Model) Validate() error {
	if err := m.Options.validate(); err != nil {
		return err
	}
	if m.SchemaVersion != 1 || m.ProjectID == "" || m.TrainingThrough.IsZero() || m.TrainingCount < 2 || m.DatasetHash == "" || len(m.TrainingGroups) == 0 || len(m.TrainingTexts) == 0 || len(m.Vocabulary) == 0 || len(m.Vocabulary) > m.Options.MaxFeatures || len(m.IDF) != len(m.Vocabulary) || len(m.Models) == 0 || len(m.Models) > 64 || len(m.Models)*len(m.Vocabulary) > 2000000 {
		return fmt.Errorf("invalid model dimensions or provenance")
	}
	m.index = map[string]int{}
	for i, t := range m.Vocabulary {
		if t == "" || (i > 0 && t <= m.Vocabulary[i-1]) || !finite(m.IDF[i]) || m.IDF[i] < 1 {
			return fmt.Errorf("invalid vocabulary or idf")
		}
		m.index[t] = i
	}
	seen := map[string]bool{}
	for _, l := range m.Models {
		if l.Label == "" || seen[l.Label] || len(l.Weights) != len(m.Vocabulary) || !finite(l.Bias) {
			return fmt.Errorf("invalid label model")
		}
		seen[l.Label] = true
		for _, w := range l.Weights {
			if !finite(w) {
				return fmt.Errorf("nonfinite weight")
			}
		}
	}
	if m.Version != m.contentVersion() {
		return fmt.Errorf("model fingerprint mismatch")
	}
	return nil
}
func Load(path string) (*Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 64<<20))
	d.DisallowUnknownFields()
	var m Model
	if err = d.Decode(&m); err != nil {
		return nil, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("expected one model document")
	}
	return &m, m.Validate()
}

// Save creates a new candidate artifact. Promotion is a separate operator action.
func (m *Model) Save(path string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > 64<<20 {
		return fmt.Errorf("serialized model exceeds 64 MB")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".classifier-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Hard linking commits atomically without replacing an existing model.
	if err = os.Link(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
