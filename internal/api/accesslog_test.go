package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestAccessLogUnary(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	a := accessLog{log: slog.New(slog.NewJSONHandler(&buf, nil))}
	req := specRequest{procedure: "/godwit.v1.GodwitService/ListRuns"}

	ok := a.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	})
	if _, err := ok(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, `"level":"INFO"`) || !strings.Contains(out, `"method":"ListRuns"`) ||
		!strings.Contains(out, `"code":"ok"`) || !strings.Contains(out, `"duration_ms":`) {
		t.Fatalf("ok line = %s", out)
	}

	buf.Reset()
	failing := a.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such run"))
	})
	if _, err := failing(context.Background(), req); err == nil {
		t.Fatal("want error")
	}
	if out := buf.String(); !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"code":"not_found"`) ||
		!strings.Contains(out, `"error":"not_found: no such run"`) {
		t.Fatalf("error line = %s", out)
	}
}

func TestAccessLogStreaming(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	a := accessLog{log: slog.New(slog.NewJSONHandler(&buf, nil))}

	called := false
	a.WrapStreamingClient(func(context.Context, connect.Spec) connect.StreamingClientConn {
		called = true

		return nil
	})(context.Background(), connect.Spec{})
	if !called || buf.Len() != 0 {
		t.Fatalf("client side must pass through silently: called=%v log=%s", called, buf.String())
	}

	handler := a.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
		return errors.New("plain")
	})
	if err := handler(context.Background(), streamConn{}); err == nil {
		t.Fatal("want error")
	}
	if out := buf.String(); !strings.Contains(out, `"method":"WatchRun"`) || !strings.Contains(out, `"code":"unknown"`) {
		t.Fatalf("stream line = %s", out)
	}
}

type specRequest struct {
	connect.AnyRequest
	procedure string
}

func (r specRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: r.procedure}
}

type streamConn struct {
	connect.StreamingHandlerConn
}

func (streamConn) Spec() connect.Spec {
	return connect.Spec{Procedure: "/godwit.v1.GodwitService/WatchRun"}
}
