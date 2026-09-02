// Package notify delivers godwit events to the outside world.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Event kinds.
const (
	KindRun   = "run"
	KindDrift = "drift"
)

// Run event types; the run's state at that moment travels in Event.State.
const (
	RunCreated          = "created"
	RunRunning          = "running"
	RunSucceeded        = "succeeded"
	RunFailed           = "failed"
	RunNeedsAttention   = "needs_attention"
	RunAwaitingContract = "awaiting_contract"
	RunConfirmed        = "confirmed"
	RunResumed          = "resumed"
	RunParked           = "parked"
	RunReverted         = "reverted"
)

// Drift event types.
const (
	DriftDetected = "detected"
	DriftResolved = "resolved"
	DriftAccepted = "accepted"
)

// Event is one notification.
type Event struct {
	Kind    string    `json:"kind"`
	Type    string    `json:"type"`
	Target  string    `json:"target"`
	RunID   string    `json:"run_id,omitempty"`
	State   string    `json:"state,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	Rollout string    `json:"rollout,omitempty"`
	Phase   string    `json:"phase,omitempty"`
	Actor   string    `json:"actor,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	At      time.Time `json:"at"`
}

// Text renders the event as one line.
func (e Event) Text() string {
	s := fmt.Sprintf("godwit %s %s on %s", e.Kind, e.Type, e.Target)
	if e.RunID != "" {
		s += " (run " + ShortID(e.RunID) + ")"
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}

	return s
}

// ShortID is the first block of a UUID, enough to tell runs apart in a message.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}

	return id
}

// Notifier delivers events.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Emit delivers e through n and logs a failure instead of returning it.
func Emit(ctx context.Context, n Notifier, log *slog.Logger, e Event) {
	if err := n.Notify(ctx, e); err != nil {
		log.Warn("notification failed", "kind", e.Kind, "type", e.Type, "target", e.Target, "run", e.RunID, "error", err)
	}
}

// None discards events.
type None struct{}

// Notify implements Notifier.
func (None) Notify(context.Context, Event) error { return nil }

// Multi fans every event out to all notifiers and joins their errors.
type Multi []Notifier

// Notify implements Notifier.
func (m Multi) Notify(ctx context.Context, e Event) error {
	var errs []error
	for _, n := range m {
		errs = append(errs, n.Notify(ctx, e))
	}

	return errors.Join(errs...)
}

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
	}{e, e.Text()})

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
