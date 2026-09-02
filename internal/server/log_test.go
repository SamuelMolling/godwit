package server

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestNewLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log, err := NewLogger(&buf, "json", "warn")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown", "k", "v")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, `"msg":"shown"`) || !strings.Contains(out, `"k":"v"`) {
		t.Fatalf("json output = %s", out)
	}

	buf.Reset()
	log, err = NewLogger(&buf, "text", "DEBUG")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("dbg")
	if out := buf.String(); !strings.Contains(out, "level=DEBUG msg=dbg") {
		t.Fatalf("text output = %s", out)
	}

	if _, err := NewLogger(&buf, "yaml", "info"); err == nil || !strings.Contains(err.Error(), `log format "yaml"`) {
		t.Fatalf("bad format err = %v", err)
	}
	if _, err := NewLogger(&buf, "json", "loud"); err == nil || !strings.Contains(err.Error(), `log level "loud"`) {
		t.Fatalf("bad level err = %v", err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func TestServiceLogs(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	sink := &lockedBuffer{}
	log, err := NewLogger(sink, "json", "info")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Listen:    "127.0.0.1:0",
			StoreDSN:  newDatabase(t, "st"),
			MasterKey: testKey,
			Tokens:    []string{"tok"},
			Holder:    "replica-log",
			Scheduler: controlplane.Config{Interval: 50 * time.Millisecond},
			Log:       log,
			OnReady:   func(addr net.Addr) { ready <- addr },
		})
	}()
	var addr net.Addr
	select {
	case addr = <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("service did not become ready")
	}
	client := newClient("http://"+addr.String(), "tok")
	const password = "s3cret-password"
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: "postgres://app:" + password + "@127.0.0.1:1/x",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := newClient("http://"+addr.String(), "").ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("err = %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	out := sink.String()
	for msg, wants := range map[string][]string{
		"store migrated":    {`"replica":"replica-log"`, `"build":"`, `"applied":`},
		"listening":         {`"addr":"` + addr.String() + `"`, `"validation":true`},
		"target registered": {`"target":"app"`, `"provider":"static"`},
		"shutting down":     {`"level":"INFO"`},
	} {
		line := logLine(t, out, `"msg":"`+msg+`"`)
		for _, want := range wants {
			if !strings.Contains(line, want) {
				t.Fatalf("%q line missing %s: %s", msg, want, line)
			}
		}
	}
	if line := logLine(t, out, `"method":"RegisterTarget"`); !strings.Contains(line, `"level":"INFO"`) || !strings.Contains(line, `"code":"ok"`) ||
		!strings.Contains(line, `"actor":"anonymous"`) || !strings.Contains(line, `"scope":"admin"`) {
		t.Fatalf("access line = %s", line)
	}
	if line := logLine(t, out, `"method":"ListRuns"`); !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, `"code":"unauthenticated"`) {
		t.Fatalf("access line = %s", line)
	}
	if strings.Contains(out, password) {
		t.Fatalf("password leaked:\n%s", out)
	}
}

func logLine(t *testing.T, out, marker string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	t.Fatalf("no log line with %s:\n%s", marker, out)

	return ""
}
