package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNone(t *testing.T) {
	t.Parallel()

	if err := (None{}).Notify(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookDelivers(t *testing.T) {
	t.Parallel()

	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := Webhook{URL: srv.URL}.Notify(context.Background(),
		Event{Kind: KindDrift, Type: DriftDetected, Target: "app", Detail: "+ column x"})
	if err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "drift" || got["type"] != "detected" || got["target"] != "app" ||
		got["text"] != "godwit drift detected on app: + column x" {
		t.Fatalf("payload = %v", got)
	}
}

func TestEventText(t *testing.T) {
	t.Parallel()

	e := Event{Kind: KindRun, Type: RunFailed, Target: "app", RunID: "12345678-abcd", Detail: "boom"}
	if got := e.Text(); got != "godwit run failed on app (run 12345678): boom" {
		t.Fatalf("text = %q", got)
	}
	if ShortID("short") != "short" {
		t.Fatal("short ids stay whole")
	}
}

type stubNotifier struct {
	events []Event
	err    error
}

func (n *stubNotifier) Notify(_ context.Context, e Event) error {
	n.events = append(n.events, e)

	return n.err
}

func TestMultiAndEmit(t *testing.T) {
	t.Parallel()

	ok, bad := &stubNotifier{}, &stubNotifier{err: errors.New("boom")}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	Emit(context.Background(), Multi{ok, bad}, log, Event{Kind: KindRun, Type: RunCreated, Target: "app", RunID: "r1"})
	if len(ok.events) != 1 || len(bad.events) != 1 {
		t.Fatalf("events = %d/%d", len(ok.events), len(bad.events))
	}
	if !strings.Contains(buf.String(), "notification failed") || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("log = %q", buf.String())
	}
	if err := (Multi{}).Notify(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if err := (Webhook{URL: srv.URL, Client: srv.Client()}).Notify(context.Background(), Event{}); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	if err := (Webhook{URL: "http://127.0.0.1:1"}).Notify(context.Background(), Event{}); err == nil ||
		!strings.Contains(err.Error(), "post webhook") {
		t.Fatalf("err = %v", err)
	}
	if err := (Webhook{URL: "://bad"}).Notify(context.Background(), Event{}); err == nil ||
		!strings.Contains(err.Error(), "build webhook request") {
		t.Fatalf("err = %v", err)
	}
}
