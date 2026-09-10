package api

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	"github.com/SamuelMolling/godwit/internal/authz"
)

// auth checks bearer tokens against the allow-set, names the caller and enforces the per-procedure scope;
// an empty set disables auth and every call runs as open, which newAuth sets explicitly.
type auth struct {
	principals map[string]authz.Principal
	open       authz.Principal
}

var anonymousAdmin = authz.Principal{Name: authz.AnonymousActor, Scope: authz.ScopeAdmin}

func newAuth(tokens []authz.Token) *auth {
	principals := map[string]authz.Principal{}
	for _, t := range tokens {
		principals[t.Secret] = authz.Principal{Name: t.Name, Scope: t.Scope}
	}

	return &auth{principals: principals, open: anonymousAdmin}
}

var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or missing bearer token"))

// actor resolves the Authorization header to a principal; ok is false when the call must be refused.
func (a *auth) actor(header string) (authz.Principal, bool) {
	if len(a.principals) == 0 {
		return a.open, true
	}
	secret, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return authz.Principal{}, false
	}
	p, ok := a.principals[secret]

	return p, ok
}

func (a *auth) authorize(ctx context.Context, procedure, header string) (context.Context, error) {
	p, ok := a.actor(header)
	if !ok {
		return ctx, errUnauthenticated
	}
	if err := authz.Authorize(procedure, p); err != nil {
		return ctx, connect.NewError(connect.CodePermissionDenied, err)
	}

	return authz.WithPrincipal(ctx, p), nil
}

// WrapUnary implements connect.Interceptor.
func (a *auth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := a.authorize(ctx, req.Spec().Procedure, req.Header().Get("Authorization"))
		if err != nil {
			return nil, err
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
		ctx, err := a.authorize(ctx, conn.Spec().Procedure, conn.RequestHeader().Get("Authorization"))
		if err != nil {
			return err
		}

		return next(ctx, conn)
	}
}
