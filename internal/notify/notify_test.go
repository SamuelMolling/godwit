package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestNone(t *testing.T) {
	t.Parallel()

	if err := (None{}).Notify(context.Background(), Event{}); err != nil {
		t.Fatal(err)
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
