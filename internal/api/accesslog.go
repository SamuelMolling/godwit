package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
)

type accessLog struct {
	log *slog.Logger
}

func (a accessLog) observe(ctx context.Context, spec connect.Spec, start time.Time, err error) {
	method := spec.Procedure[strings.LastIndex(spec.Procedure, "/")+1:]
	code, level := "ok", slog.LevelInfo
	var extra []any
	if err != nil {
		code, level = connect.CodeOf(err).String(), slog.LevelWarn
		extra = []any{"error", err.Error()}
	}
	attrs := append([]any{"method", method, "code", code, "duration_ms", time.Since(start).Milliseconds()}, extra...)
	a.log.Log(ctx, level, "api call", attrs...)
}

// WrapUnary implements connect.Interceptor.
func (a accessLog) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		a.observe(ctx, req.Spec(), start, err)

		return resp, err
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (accessLog) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (a accessLog) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		a.observe(ctx, conn.Spec(), start, err)

		return err
	}
}
