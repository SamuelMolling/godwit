// Package notify delivers godwit events to the outside world.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Event is one notification.
type Event struct {
	Type   string `json:"type"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// Notifier delivers events.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// None discards events.
type None struct{}

// Notify implements Notifier.
func (None) Notify(context.Context, Event) error { return nil }

// Webhook POSTs events as JSON to a URL.
type Webhook struct {
	URL    string
	Client *http.Client
}

// Notify implements Notifier.
func (w Webhook) Notify(ctx context.Context, e Event) error {
	body, _ := json.Marshal(struct {
		Event
		Text string `json:"text"`
	}{e, fmt.Sprintf("godwit %s on %s: %s", e.Type, e.Target, e.Detail)})

	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}

	return nil
}
