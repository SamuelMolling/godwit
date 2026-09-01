package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeClosedListener(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if err := serve(&http.Server{ReadHeaderTimeout: time.Second}, ln); err == nil {
		t.Fatal("closed listener must surface an error")
	}
}

func TestRunWithoutOnReadyShutsDownCleanly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			Listen:         "127.0.0.1:0",
			StoreDSN:       newDatabase(t, "st"),
			WebhookURL:     "http://127.0.0.1:1/hook",
			SkipValidation: true,
			Log:            testLog,
		})
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run() did not shut down")
	}
}
