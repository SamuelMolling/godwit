package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries the delivery's timestamp and one HMAC-SHA256 of it and the body per secret.
const SignatureHeader = "Godwit-Signature"

// Webhook POSTs events as JSON to a URL, with one signature per secret in Secrets.
type Webhook struct {
	URL     string
	Secrets []string
	Client  *http.Client
	now     func() time.Time
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
	now := w.now
	if now == nil {
		now = time.Now
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, sign(w.Secrets, now(), body))
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

func sign(secrets []string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	parts := []string{"t=" + ts}
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "."))
		mac.Write(body)
		parts = append(parts, "v1="+hex.EncodeToString(mac.Sum(nil)))
	}

	return strings.Join(parts, ",")
}
