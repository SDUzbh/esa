package esa

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"esa/internal/classifier"
)

// The intake-time subject and description are the only predictive fields.
// Resolutions, labels, agent reports and later comments are never features.
func classifierText(t Ticket) string { return t.Subject + "\n" + t.Description }
func (api *API) classifierTicket(r *http.Request) (Ticket, error) {
	t, ok := api.Engine.Store.Snapshot().Tickets[r.PathValue("id")]
	if !ok || t.Private || t.ForwardedBy == "" || t.ForwardedAt.IsZero() || !allowed(r, t.ProjectID) || !ticketAllowed(r, t.ID) {
		return t, ErrNotFound
	}
	return t, nil
}
func latestPrediction(s State, t Ticket, m *classifier.Model) *ClassifierPrediction {
	var latest *ClassifierPrediction
	for _, p := range s.ClassifierPredictions {
		if p.TicketID == t.ID && p.ProjectID == t.ProjectID && p.Text == classifierText(t) && p.ForwardedAt.Equal(t.ForwardedAt) && p.Prediction.ModelVersion == m.Version {
			copy := p
			if latest == nil || p.CreatedAt.After(latest.CreatedAt) {
				latest = &copy
			}
		}
	}
	return latest
}
func (api *API) classifierStatus(w http.ResponseWriter, r *http.Request) {
	t, err := api.classifierTicket(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, api.Engine.classifierView(api.Engine.Store.Snapshot(), t))
}

func (e *Engine) classifierView(s State, t Ticket) map[string]any {
	m := e.classifiers[t.ProjectID]
	if !e.Config.Classifier.Enabled || m == nil {
		return map[string]any{"enabled": false}
	}
	p := latestPrediction(s, t, m)
	var feedback *ClassifierFeedback
	if p != nil {
		for _, f := range s.ClassifierFeedback {
			if f.PredictionID == p.ID && (feedback == nil || f.Example.ReviewedAt.After(feedback.Example.ReviewedAt) || f.Example.ReviewedAt.Equal(feedback.Example.ReviewedAt) && f.ID > feedback.ID) {
				copy := f
				feedback = &copy
			}
		}
	}
	blocked := ""
	if m.TrainingThrough.After(t.ForwardedAt) {
		blocked = "Model contains reviews after this forwarded snapshot; use an earlier model or forward a current snapshot."
	}
	return map[string]any{"enabled": true, "labels": m.Labels(), "model_version": m.Version, "training_through": m.TrainingThrough, "blocked_reason": blocked, "prediction": p, "feedback": feedback}
}
func (api *API) classifierPredict(w http.ResponseWriter, r *http.Request) {
	t, err := api.classifierTicket(r)
	if err != nil {
		writeError(w, err)
		return
	}
	m := api.Engine.classifiers[t.ProjectID]
	if !api.Engine.Config.Classifier.Enabled || m == nil {
		writeError(w, fmt.Errorf("%w: classifier disabled for this project", ErrConflict))
		return
	}
	if m.TrainingThrough.After(t.ForwardedAt) {
		writeError(w, fmt.Errorf("%w: model training is later than ticket snapshot", ErrConflict))
		return
	}
	text := classifierText(t)
	if len(text) > 1<<20 {
		writeError(w, fmt.Errorf("%w: classifier text exceeds 1 MB", ErrInvalid))
		return
	}
	p := ClassifierPrediction{TicketID: t.ID, ProjectID: t.ProjectID, Text: text, ForwardedAt: t.ForwardedAt, CreatedAt: api.Engine.Now(), Prediction: m.Predict(text)}
	p.ID = hash([]string{t.ID, t.ProjectID, text, t.ForwardedAt.UTC().Format(time.RFC3339Nano), m.Version})
	err = api.Engine.update(func(s *State) error {
		current, ok := s.Tickets[t.ID]
		if !ok || current.Private || current.ProjectID != t.ProjectID || classifierText(current) != text || !current.ForwardedAt.Equal(t.ForwardedAt) {
			return ErrConflict
		}
		if old, ok := s.ClassifierPredictions[p.ID]; ok {
			p = old
			return nil
		}
		if s.ClassifierPredictions == nil {
			s.ClassifierPredictions = map[string]ClassifierPrediction{}
		}
		s.ClassifierPredictions[p.ID] = p
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, p)
}

type classifierFeedbackRequest struct {
	ID           string   `json:"id"`
	PredictionID string   `json:"prediction_id"`
	Labels       []string `json:"labels"`
	Reviewer     string   `json:"reviewer"`
	Note         string   `json:"note"`
}

func (api *API) classifierFeedback(w http.ResponseWriter, r *http.Request) {
	t, err := api.classifierTicket(r)
	if err != nil {
		writeError(w, err)
		return
	}
	m := api.Engine.classifiers[t.ProjectID]
	if !api.Engine.Config.Classifier.Enabled || m == nil {
		writeError(w, fmt.Errorf("%w: classifier disabled", ErrConflict))
		return
	}
	var req classifierFeedbackRequest
	if err = decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(req.ID) == "" || len(req.ID) > 200 || strings.TrimSpace(req.Reviewer) == "" || len(req.Reviewer) > 200 || len(req.Note) > 4000 || req.Labels == nil {
		writeError(w, fmt.Errorf("%w: id, reviewer and explicit labels are required", ErrInvalid))
		return
	}
	known := map[string]bool{}
	for _, l := range m.Labels() {
		known[l] = true
	}
	seen := map[string]bool{}
	for _, l := range req.Labels {
		if !known[l] || seen[l] {
			writeError(w, fmt.Errorf("%w: unknown or duplicate label", ErrInvalid))
			return
		}
		seen[l] = true
	}
	sort.Strings(req.Labels)
	var record ClassifierFeedback
	err = api.Engine.update(func(s *State) error {
		current, ok := s.Tickets[t.ID]
		if !ok || current.Private || current.ProjectID != t.ProjectID {
			return ErrNotFound
		}
		p, ok := s.ClassifierPredictions[req.PredictionID]
		if !ok || p.TicketID != t.ID || p.ProjectID != t.ProjectID {
			return ErrNotFound
		}
		if p.Prediction.ModelVersion != m.Version || p.Text != classifierText(current) || !p.ForwardedAt.Equal(current.ForwardedAt) || m.TrainingThrough.After(p.ForwardedAt) {
			return fmt.Errorf("%w: prediction is stale; request suggestions for the current snapshot", ErrConflict)
		}
		key := hash([]string{t.ProjectID, t.ID, req.ID})
		if old, ok := s.ClassifierFeedback[key]; ok {
			if old.PredictionID != req.PredictionID || old.Example.Reviewer != req.Reviewer || hash(old.Example.Labels) != hash(req.Labels) || old.Note != req.Note {
				return ErrConflict
			}
			record = old
			return nil
		}
		reviewedAt := api.Engine.Now()
		for _, previous := range s.ClassifierFeedback {
			if previous.Example.ID == t.ID && previous.Example.ProjectID == t.ProjectID && !reviewedAt.After(previous.Example.ReviewedAt) {
				return fmt.Errorf("%w: review timestamp must advance; retry after the server clock advances", ErrConflict)
			}
		}
		record = ClassifierFeedback{ID: key, PredictionID: p.ID, ModelVersion: p.Prediction.ModelVersion, Note: req.Note, Example: classifier.Example{ID: t.ID, ProjectID: t.ProjectID, Text: p.Text, Labels: req.Labels, Reviewer: req.Reviewer, ReviewedAt: reviewedAt}}
		if err := record.Example.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if s.ClassifierFeedback == nil {
			s.ClassifierFeedback = map[string]ClassifierFeedback{}
		}
		s.ClassifierFeedback[key] = record
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, record)
}
func (api *API) classifierExport(w http.ResponseWriter, r *http.Request) {
	if !api.Engine.Config.Classifier.Enabled {
		writeError(w, fmt.Errorf("%w: classifier disabled", ErrConflict))
		return
	}
	before, err := time.Parse(time.RFC3339, r.URL.Query().Get("before"))
	if err != nil || before.After(api.Engine.Now()) {
		writeError(w, fmt.Errorf("%w: before must be RFC3339 and no later than now", ErrInvalid))
		return
	}
	s := api.Engine.Store.Snapshot()
	latest := map[string]ClassifierFeedback{}
	for _, f := range s.ClassifierFeedback {
		t, ok := s.Tickets[f.Example.ID]
		if !ok || t.Private || t.ProjectID != f.Example.ProjectID || !allowed(r, t.ProjectID) || !ticketAllowed(r, t.ID) || f.Example.ReviewedAt.After(before) {
			continue
		}
		key := hash([]string{t.ProjectID, t.ID})
		old, ok := latest[key]
		if !ok || f.Example.ReviewedAt.After(old.Example.ReviewedAt) || f.Example.ReviewedAt.Equal(old.Example.ReviewedAt) && f.ID > old.ID {
			latest[key] = f
		}
	}
	keys := []string{}
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	for _, key := range keys {
		if err := enc.Encode(latest[key].Example); err != nil {
			return
		}
	}
}
