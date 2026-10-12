package esa

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"esa/internal/classifier"
)

func classifierEngine(t *testing.T) *Engine {
	t.Helper()
	e := testEngine(t)
	f, err := os.Open("../../examples/classifier/train.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	es, err := classifier.ReadExamples(f)
	if err != nil {
		t.Fatal(err)
	}
	m, err := classifier.Train(context.Background(), es, classifier.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if err = m.Save(path); err != nil {
		t.Fatal(err)
	}
	c := e.Config
	c.Classifier = ClassifierConfig{Enabled: true, Models: map[string]string{"1": path}}
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(store, c)
	if err != nil {
		t.Fatal(err)
	}
	engine.Now = e.Now
	return engine
}
func classifierRequest(e *Engine, method, path, body, scope, issues string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("X-ESA-Project-IDs", scope)
	r.Header.Set("X-ESA-Ticket-IDs", issues)
	w := httptest.NewRecorder()
	(&API{Engine: e, Token: "token"}).Handler().ServeHTTP(w, r)
	return w
}
func TestClassifierDisabledByDefaultAndNotARoutingGate(t *testing.T) {
	e := testEngine(t)
	c := e.Config
	c.Classifier.Models = map[string]string{"1": "/file/does/not/exist"}
	if _, err := NewEngine(e.Store, c); err != nil {
		t.Fatal("disabled mode loaded model", err)
	}
	receive(t, e, "forward", fixture(e, "ticket"))
	s := e.Store.Snapshot()
	w := classifierRequest(e, "POST", "/v1/tickets/ticket/classifier/predict", "{}", "1", "ticket")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	w = classifierRequest(e, "GET", "/v1/tickets/ticket/classifier", "", "1", "ticket")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatal(w.Code, w.Body)
	}
	if hash(s) != hash(e.Store.Snapshot()) {
		t.Fatal("disabled prediction changed state")
	}
	c.Classifier.Enabled = true
	if _, err := NewEngine(e.Store, c); err == nil {
		t.Fatal("explicit invalid model enabled")
	}
}
func TestClassifierPredictionHumanFeedbackExportAndPersistence(t *testing.T) {
	e := classifierEngine(t)
	ticket := fixture(e, "ticket")
	ticket.Subject = "Consumer crash slow latency throughput"
	ticket.Description = "Null pointer regression reproduced"
	receive(t, e, "forward", ticket)
	before := e.Store.Snapshot()
	if len(before.ClassifierPredictions) != 0 {
		t.Fatal("intake automatically invoked classifier")
	}
	w := classifierRequest(e, "POST", "/v1/tickets/ticket/classifier/predict", "{}", "1", "ticket")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var p ClassifierPrediction
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Prediction.Labels) != 2 || strings.Contains(p.Text, "JDK 17") {
		t.Fatal("multilabel or feature contract broken", p)
	}
	req := classifierFeedbackRequest{ID: "feedback-1", PredictionID: p.ID, Labels: []string{"bug", "usage"}, Reviewer: "engineer", Note: "Confirmed from ticket"}
	b, _ := json.Marshal(req)
	for i := 0; i < 2; i++ {
		w = classifierRequest(e, "POST", "/v1/tickets/ticket/classifier/feedback", string(b), "1", "ticket")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	s := e.Store.Snapshot()
	if len(s.ClassifierFeedback) != 1 || hash(before.Runs) != hash(s.Runs) || hash(before.Tickets) != hash(s.Tickets) || hash(before.Cases) != hash(s.Cases) {
		t.Fatal("feedback changed workflow or was duplicated")
	}
	w = classifierRequest(e, "GET", "/v1/classifier/feedback?before="+e.Now().Format(time.RFC3339), "", "1", "ticket")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	es, err := classifier.ReadExamples(w.Body)
	if err != nil || len(es) != 1 || es[0].Reviewer != "engineer" || es[0].Text != p.Text {
		t.Fatal("feedback export incorrect", es, err)
	}
	for _, query := range []struct{ scope, issues string }{{"2", "ticket"}, {"1", "other"}} {
		w = classifierRequest(e, "GET", "/v1/classifier/feedback?before="+e.Now().Format(time.RFC3339), "", query.scope, query.issues)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal("feedback crossed visibility", w.Code, w.Body)
		}
	}
	w = classifierRequest(e, "GET", "/v1/tickets/ticket", "", "1", "ticket")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reviewer":"engineer"`) {
		t.Fatal("ticket view missing human feedback", w.Body)
	}
	loaded, err := OpenStore(e.Store.path)
	if err != nil || len(loaded.Snapshot().ClassifierFeedback) != 1 {
		t.Fatal("feedback not durable", err)
	}
	req.Labels = []string{"unsupported"}
	b, _ = json.Marshal(req)
	w = classifierRequest(e, "POST", "/v1/tickets/ticket/classifier/feedback", string(b), "1", "ticket")
	if w.Code != 400 {
		t.Fatal("unknown label accepted", w.Code)
	}
	req.Labels = []string{"usage"}
	b, _ = json.Marshal(req)
	w = classifierRequest(e, "POST", "/v1/tickets/ticket/classifier/feedback", string(b), "1", "ticket")
	if w.Code != 409 {
		t.Fatal("conflicting idempotency id accepted", w.Code)
	}
	firstReview := e.Now()
	e.Now = func() time.Time { return firstReview.Add(time.Second) }
	req.ID = "feedback-correction"
	req.Labels = []string{}
	b, _ = json.Marshal(req)
	w = classifierRequest(e, "POST", "/v1/tickets/ticket/classifier/feedback", string(b), "1", "ticket")
	if w.Code != 200 {
		t.Fatal("explicit negative feedback rejected", w.Code, w.Body)
	}
	for _, cutoff := range []time.Time{firstReview, e.Now()} {
		w = classifierRequest(e, "GET", "/v1/classifier/feedback?before="+cutoff.Format(time.RFC3339), "", "1", "ticket")
		es, err = classifier.ReadExamples(w.Body)
		if err != nil || len(es) != 1 {
			t.Fatal("latest feedback export invalid", err)
		}
		if cutoff.Equal(firstReview) && len(es[0].Labels) != 2 || !cutoff.Equal(firstReview) && len(es[0].Labels) != 0 {
			t.Fatal("feedback cutoff or latest correction incorrect", es)
		}
	}
	oldNow := e.Now()
	e.Now = func() time.Time { return oldNow.Add(time.Hour) }
	ticket.Description = "Changed diagnostic text"
	ticket.UpdatedAt = e.Now()
	receive(t, e, "forward-new", ticket)
	req.ID = "feedback-2"
	b, _ = json.Marshal(req)
	w = classifierRequest(e, "POST", "/v1/tickets/ticket/classifier/feedback", string(b), "1", "ticket")
	if w.Code != 409 {
		t.Fatal("stale prediction accepted", w.Code)
	}
}
func TestClassifierFutureModelAndPrivateVisibility(t *testing.T) {
	e := classifierEngine(t)
	ticket := fixture(e, "historical")
	ticket.CreatedAt = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ticket.UpdatedAt = ticket.CreatedAt
	if _, err := e.Receive(Event{ID: "historical", Origin: "test", ForwardedBy: "human", ForwardedAt: ticket.CreatedAt, Ticket: ticket}); err != nil {
		t.Fatal(err)
	}
	w := classifierRequest(e, "POST", "/v1/tickets/historical/classifier/predict", "{}", "1", "historical")
	if w.Code != 409 || len(e.Store.Snapshot().ClassifierPredictions) != 0 {
		t.Fatal("future model used in replay", w.Code, w.Body)
	}
	for _, path := range []string{"/v1/tickets/historical/classifier", "/v1/tickets/historical/classifier/predict"} {
		method := "GET"
		if strings.HasSuffix(path, "predict") {
			method = "POST"
		}
		w = classifierRequest(e, method, path, "{}", "2", "historical")
		if w.Code != 404 {
			t.Fatal("project scope bypassed", w.Code)
		}
	}
	e.Store.Update(func(s *State) error { t := s.Tickets["historical"]; t.Private = true; s.Tickets[t.ID] = t; return nil })
	w = classifierRequest(e, "GET", "/v1/tickets/historical/classifier", "", "1", "historical")
	if w.Code != 404 {
		t.Fatal("private prediction visible", w.Code)
	}
}
