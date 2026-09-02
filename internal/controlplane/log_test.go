package controlplane

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.String()
}

func captureLog() (*logSink, *slog.Logger) {
	sink := &logSink{}

	return sink, slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestPGEngineObserverLogs(t *testing.T) {
	t.Parallel()

	sink, log := captureLog()
	observe := PGEngine{Log: log}.observer("run-1", "app")
	observe(engine.StatementEvent{Version: 7, Index: 2, Statement: engine.Statement{SQL: "INSERT INTO people VALUES ('pii')"}})
	observe(engine.StatementEvent{Version: 7, Index: 3, Statement: engine.Statement{SQL: "SELECT secret_column", NoTx: true}, Err: errors.New("boom")})

	out := sink.String()
	for _, want := range []string{
		`"msg":"statement applied"`, `"run":"run-1"`, `"target":"app"`, `"version":7`, `"stmt":2`, `"kind":"tx"`, `"duration_ms":0`,
		`"msg":"statement failed"`, `"stmt":3`, `"kind":"no_tx"`, `"error":"boom"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s:\n%s", want, out)
		}
	}
	for _, leak := range []string{"pii", "people", "secret_column"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaks statement text %q:\n%s", leak, out)
		}
	}
}

func TestSchedulerNeverLogsPassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	const password = "hunter2-top-secret"
	dsn := "postgres://app:" + password + "@127.0.0.1:1/x"
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": dsn}); err != nil {
		t.Fatal(err)
	}
	const id = "33333333-0000-0000-0000-000000000001"
	queueRun(t, s, id, goodFiles())

	sink, log := captureLog()
	sched := NewScheduler(s, map[string]creds.Provider{"plain": plainProvider{}}, PGEngine{Log: log}, Policies(), Config{Holder: "h", MaxAttempts: 1}, log)
	sched.Tick(ctx)
	r := waitState(t, s, id, StateNeedsAttention)

	out := sink.String()
	if !strings.Contains(out, `"msg":"run claimed"`) || !strings.Contains(out, `"msg":"run finished"`) ||
		!strings.Contains(out, `"state":"needs_attention"`) || !strings.Contains(out, `"run":"`+id+`"`) {
		t.Fatalf("lifecycle lines missing:\n%s", out)
	}
	if strings.Contains(out, password) || strings.Contains(r.Error, password) {
		t.Fatalf("password leaked:\n%s\n%s", out, r.Error)
	}
}
