package esa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SyncRedmine drains the plugin's durable outbox and acknowledges only committed events.
// SyncRedmine polls the manual-forwarding outbox and acknowledges only committed events.
// An acknowledgement failure leaves delivery retryable through event idempotency.
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
	for _, event := range payload.Events {
		if _, err = e.Receive(event); err != nil {
			return fmt.Errorf("event %s: %w", event.ID, err)
		}
		body, _ := json.Marshal(map[string]string{"event_id": event.ID})
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
	}
	return nil
}
