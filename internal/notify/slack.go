package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Slack message modes.
const (
	ModeThread = "thread"
	ModeEdit   = "edit"
)

// SlackAPI is the default Slack Web API base.
const SlackAPI = "https://slack.com/api"

const slackDetailLimit = 500

// TSStore remembers which Slack message a run or drift key maps to, across replicas.
type TSStore interface {
	GetTS(ctx context.Context, kind, key string) (channel, ts string, err error)
	PutTS(ctx context.Context, kind, key, channel, ts string) error
}

// Slack posts one Block Kit message per run (or drift detection) and keeps it current.
type Slack struct {
	Token   string
	Channel string
	// Mode is thread (root message plus threaded replies, root kept current) or edit (one message rewritten).
	Mode    string
	Client  *http.Client
	Store   TSStore
	BaseURL string
	// PublicURL, when set, adds an "Open run" button pointing at the UI.
	PublicURL string
	// Sleep is the retry backoff wait; nil means a context-aware time.Sleep.
	Sleep func(ctx context.Context, d time.Duration) error
}

var slackBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

type slackResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

type retryError struct {
	err   error
	after time.Duration
}

func (e *retryError) Error() string { return e.err.Error() }

// Notify implements Notifier.
func (s Slack) Notify(ctx context.Context, e Event) error {
	key := e.RunID
	if e.Kind == KindDrift {
		key = "drift:" + e.Target
	}
	channel, ts, err := s.Store.GetTS(ctx, e.Kind, key)
	if err != nil {
		return err
	}
	if ts == "" || (e.Kind == KindDrift && e.Type == DriftDetected) {
		out, err := s.call(ctx, "chat.postMessage", map[string]any{
			"channel": s.Channel, "text": e.Text(), "blocks": s.blocks(e),
		})
		if err != nil {
			return err
		}

		return s.Store.PutTS(ctx, e.Kind, key, out.Channel, out.TS)
	}
	if s.Mode != ModeEdit {
		if _, err := s.call(ctx, "chat.postMessage", map[string]any{
			"channel": channel, "thread_ts": ts, "text": e.Text(), "blocks": replyBlocks(e),
		}); err != nil {
			return err
		}
	}
	_, err = s.call(ctx, "chat.update", map[string]any{
		"channel": channel, "ts": ts, "text": e.Text(), "blocks": s.blocks(e),
	})

	return err
}

func (s Slack) call(ctx context.Context, method string, payload map[string]any) (slackResponse, error) {
	body, _ := json.Marshal(payload)
	for attempt := 0; ; attempt++ {
		out, err := s.post(ctx, method, body)
		var re *retryError
		if err == nil || !errors.As(err, &re) || attempt == len(slackBackoff) {
			return out, err
		}
		wait := slackBackoff[attempt]
		if re.after > 0 {
			wait = re.after
		}
		if err := s.sleep(ctx, wait); err != nil {
			return out, fmt.Errorf("%v: %w", re.err, err)
		}
	}
}

func (s Slack) post(ctx context.Context, method string, body []byte) (slackResponse, error) {
	var out slackResponse
	base := s.BaseURL
	if base == "" {
		base = SlackAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, &retryError{err: fmt.Errorf("post slack %s: %w", method, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))

		return out, &retryError{err: fmt.Errorf("slack %s rate limited", method), after: time.Duration(after) * time.Second}
	case resp.StatusCode >= 500:
		return out, &retryError{err: fmt.Errorf("slack %s returned %s", method, resp.Status)}
	case resp.StatusCode >= 300:
		return out, fmt.Errorf("slack %s returned %s", method, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode slack %s response: %w", method, err)
	}
	if !out.OK {
		return out, fmt.Errorf("slack %s: %s", method, out.Error)
	}

	return out, nil
}

func (s Slack) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var stateIcons = map[string]string{
	"queued":            "🕐",
	"running":           "🔁",
	"succeeded":         "✅",
	"failed":            "❌",
	"needs_attention":   "🛑",
	"awaiting_contract": "⏸",
	"reverted":          "⏪",
}

func badge(e Event) string {
	if e.Kind == KindDrift {
		if e.Type == DriftDetected {
			return "🧭 drift detected"
		}

		return "✅ drift " + e.Type
	}

	return stateIcons[e.State] + " " + strings.ReplaceAll(e.State, "_", " ")
}

func mrkdwn(text string) map[string]any {
	return map[string]any{"type": "mrkdwn", "text": text}
}

func (s Slack) blocks(e Event) []map[string]any {
	title := "godwit · " + e.Target + " · drift"
	if e.Kind == KindRun {
		title = "godwit · " + e.Target + " · " + ShortID(e.RunID)
	}
	fields := []map[string]any{mrkdwn("*State*\n" + badge(e))}
	if e.Rollout != "" {
		fields = append(fields, mrkdwn("*Rollout*\n"+e.Rollout+" / "+e.Phase))
	}
	if e.Attempt > 0 {
		fields = append(fields, mrkdwn("*Attempt*\n"+strconv.Itoa(e.Attempt)))
	}
	if e.Actor != "" {
		fields = append(fields, mrkdwn("*Actor*\n"+e.Actor))
	}
	fields = append(fields, mrkdwn("*Last event*\n"+e.Type+" · "+e.At.UTC().Format(time.RFC3339)))
	out := []map[string]any{
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": title}},
		{"type": "section", "fields": fields},
	}
	if e.Detail != "" {
		out = append(out, map[string]any{"type": "section", "text": mrkdwn("```" + truncate(e.Detail) + "```")})
	}
	if s.PublicURL != "" && e.RunID != "" {
		out = append(out, map[string]any{"type": "actions", "elements": []map[string]any{{
			"type": "button",
			"text": map[string]any{"type": "plain_text", "text": "Open run"},
			"url":  strings.TrimRight(s.PublicURL, "/") + "/ui/runs/" + e.RunID,
		}}})
	}

	return out
}

func replyBlocks(e Event) []map[string]any {
	text := badge(e) + " · *" + e.Type + "*"
	if e.Attempt > 0 {
		text += " · attempt " + strconv.Itoa(e.Attempt)
	}
	if e.Actor != "" {
		text += " · by " + e.Actor
	}
	if e.Detail != "" {
		text += "\n```" + truncate(e.Detail) + "```"
	}

	return []map[string]any{{"type": "section", "text": mrkdwn(text)}}
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= slackDetailLimit {
		return s
	}

	return string(r[:slackDetailLimit]) + "…"
}
