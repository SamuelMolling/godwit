package notify

import (
	"context"
	"encoding/json"
	"io"
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
		Event{Type: "drift", Target: "app", Detail: "+ column x"})
	if err != nil {
		t.Fatal(err)
	}
	if got["type"] != "drift" || got["target"] != "app" || !strings.Contains(got["text"], "godwit drift on app") {
		t.Fatalf("payload = %v", got)
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
