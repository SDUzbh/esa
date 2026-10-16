package esa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// SyncRedmine polls the manual-forwarding outbox and acknowledges only committed events.
// A permanently rejected event is recorded, then acknowledged so it cannot block
// later events. A transient failure stays unacknowledged and holds back only later
// events of the same ticket, preserving per-ticket order. An acknowledgement
// failure leaves delivery retryable through event idempotency.
func (e *Engine) SyncRedmine(ctx context.Context, baseURL, token string, client *http.Client) error {
	if baseURL == "" {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/esa/api/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Redmine outbox returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Events []Event `json:"events"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 12<<20)).Decode(&payload); err != nil {
		return err
	}
	held := map[string]bool{}
	var failures []error
	for _, event := range payload.Events {
		if held[event.Ticket.ID] {
			continue
		}
		_, err = e.Receive(event)
		if errors.Is(err, ErrRejected) {
			slog.Warn("Forwarded event rejected", "event_id", event.ID, "ticket_id", event.Ticket.ID, "forwarded_by", event.ForwardedBy, "reason", err)
			err = e.recordRejection(event, err)
		}
		if err != nil {
			held[event.Ticket.ID] = true
			failures = append(failures, fmt.Errorf("event %s: %w", event.ID, err))
			continue
		}
		if err = acknowledge(ctx, client, baseURL, token, event.ID); err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	return errors.Join(failures...)
}

// recordRejection keeps the first rejection of an event ID; redelivery after a
// lost acknowledgement re-evaluates the same frozen event and adds nothing.
func (e *Engine) recordRejection(event Event, cause error) error {
	return e.Store.Update(func(s *State) error {
		if _, ok := s.Rejections[event.ID]; ok {
			return nil
		}
		if s.Rejections == nil {
			s.Rejections = map[string]EventRejection{}
		}
		s.Rejections[event.ID] = EventRejection{EventID: event.ID, TicketID: event.Ticket.ID, ProjectID: event.Ticket.ProjectID, ForwardedBy: event.ForwardedBy, ForwardedAt: event.ForwardedAt, Reason: cause.Error(), RejectedAt: e.Now()}
		return nil
	})
}

func acknowledge(ctx context.Context, client *http.Client, baseURL, token, eventID string) error {
	body, _ := json.Marshal(map[string]string{"event_id": eventID})
	ack, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/esa/api/events/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	ack.Header.Set("Authorization", "Bearer "+token)
	ack.Header.Set("Content-Type", "application/json")
	response, err := client.Do(ack)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("Redmine acknowledgement returned HTTP %d", response.StatusCode)
	}
	return nil
}
