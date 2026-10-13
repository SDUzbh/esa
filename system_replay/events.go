package systemreplay

import (
	"context"
	"fmt"
)

func checked(value any, expected []Assertion, message string) ([]Check, error) {
	checks, err := assertions(value, expected)
	if err == nil && len(expected) > 0 && !passed(checks) {
		err = fmt.Errorf("%s", message)
	}
	return checks, err
}

// Captured identifiers are observations, not decisions supplied to the target.
// Rendering assertion values lets a manifest check shared identities without
// predicting IDs or implementing the target's aggregation algorithm.
func checkedCaptured(value any, expected []Assertion, vars map[string]any, message string) ([]Check, error) {
	resolved := append([]Assertion(nil), expected...)
	for index := range resolved {
		v, err := render(resolved[index].Value, vars)
		if err != nil {
			return nil, err
		}
		resolved[index].Value = v
	}
	return checked(value, resolved, message)
}

// Every event follows the same public submit/observe path. No business decisions
// or assertion labels are included in the request template variables.
func (c *Client) event(ctx context.Context, event Event, vars map[string]any) (record EventResult) {
	record.At = event.At
	var err error
	defer func() {
		if err != nil {
			record.Error = err.Error()
		}
	}()
	vars["event_time"] = event.At
	if event.ID != "" {
		vars["event_id"] = event.ID
	}
	if event.Ticket != nil {
		vars["ticket"] = clone(event.Ticket)
		vars["ticket_id"] = event.Ticket["id"]
		vars["source_key"] = event.Ticket["source_key"]
		vars["project_id"] = vars["default_project"]
		if project := event.Ticket["project_id"]; project != nil {
			vars["project_id"] = project
		}
	}
	if c.target.AdvanceClock != nil {
		_, err = c.accept(ctx, *c.target.AdvanceClock, vars)
		if err != nil {
			return
		}
	}
	request := c.target.Submit
	if event.Request != nil {
		request = *event.Request
	}
	record.Receipt, err = c.request(ctx, request, vars)
	if err != nil {
		return
	}
	vars["receipt"] = record.Receipt.Body
	record.Checks, err = checkedCaptured(map[string]any{"status": record.Receipt.Status, "body": record.Receipt.Body}, event.ReceiptExpect, vars, "public submission response assertion failed")
	if err == nil && len(event.ReceiptExpect) == 0 && record.Receipt.Status >= 300 {
		err = fmt.Errorf("submission returned HTTP %d", record.Receipt.Status)
	}
	if err != nil || event.SkipObserve {
		return
	}
	observe := c.target.Observe
	if event.Observe != nil {
		observe = *event.Observe
	}
	record.Observation, err = c.poll(ctx, observe, vars)
	if err != nil {
		return
	}
	var checks []Check
	checks, err = checkedCaptured(observationValue(record.Observation), event.Expect, vars, "observed system result failed assertions")
	record.Checks = append(record.Checks, checks...)
	for key, path := range c.target.Capture {
		delete(vars, key)
		if value, exists := pointer(record.Observation.Raw, path); exists {
			vars[key] = value
		}
	}
	return
}

func (c *Client) accept(ctx context.Context, request Request, vars map[string]any) (HTTPRecord, error) {
	record, err := c.request(ctx, request, vars)
	if err == nil && record.Status >= 300 {
		err = fmt.Errorf("%s returned HTTP %d", record.Path, record.Status)
	}
	return record, err
}

func initialObservation(result Result) Observation {
	if len(result.Events) > 0 {
		return result.Events[0].Observation
	}
	return Observation{}
}
