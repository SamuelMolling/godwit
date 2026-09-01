package api

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
)

// auth rejects requests whose bearer token is not in the allow-set.
// An empty set disables authentication (dev mode).
type auth struct {
	tokens map[string]bool
}

func newAuth(tokens []string) *auth {
	set := map[string]bool{}
	for _, t := range tokens {
		set[t] = true
	}

	return &auth{tokens: set}
}

var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or missing bearer token"))

func (a *auth) allowed(header string) bool {
	if len(a.tokens) == 0 {
		return true
	}
	token, ok := strings.CutPrefix(header, "Bearer ")

	return ok && a.tokens[token]
}

// WrapUnary implements connect.Interceptor.
func (a *auth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !a.allowed(req.Header().Get("Authorization")) {
			return nil, errUnauthenticated
		}

		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (a *auth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (a *auth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if !a.allowed(conn.RequestHeader().Get("Authorization")) {
			return errUnauthenticated
		}

		return next(ctx, conn)
	}
}
