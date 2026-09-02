package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
)

// AnonymousActor names calls made with an unnamed token or against a service without tokens.
const AnonymousActor = "anonymous"

// Token is one accepted bearer secret and the actor name it resolves to.
type Token struct {
	Name   string
	Secret string
}

// ParseTokens reads token specs of the form "name:secret"; a bare "secret" is named anonymous.
func ParseTokens(specs []string) ([]Token, error) {
	seen := map[string]string{}
	out := make([]Token, 0, len(specs))
	for i, spec := range specs {
		name, secret, ok := strings.Cut(strings.TrimSpace(spec), ":")
		if !ok {
			name, secret = AnonymousActor, name
		}
		if name == "" || secret == "" {
			return nil, fmt.Errorf("token #%d: want name:secret or a bare secret", i+1)
		}
		if other, dup := seen[secret]; dup {
			return nil, fmt.Errorf("token #%d (%s): secret already used by %s", i+1, name, other)
		}
		seen[secret] = name
		out = append(out, Token{Name: name, Secret: secret})
	}

	return out, nil
}

type actorKey struct{}

// Actor returns the name of the token behind the call, or anonymous outside an authenticated request.
func Actor(ctx context.Context) string {
	if name, ok := ctx.Value(actorKey{}).(string); ok {
		return name
	}

	return AnonymousActor
}

// auth checks bearer tokens against the allow-set and names the caller; an empty set disables auth.
type auth struct {
	names map[string]string
}

func newAuth(tokens []Token) *auth {
	names := map[string]string{}
	for _, t := range tokens {
		names[t.Secret] = t.Name
	}

	return &auth{names: names}
}

var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or missing bearer token"))

// actor resolves the Authorization header to an actor name; ok is false when the call must be refused.
func (a *auth) actor(header string) (string, bool) {
	if len(a.names) == 0 {
		return AnonymousActor, true
	}
	secret, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", false
	}
	name, ok := a.names[secret]

	return name, ok
}

// WrapUnary implements connect.Interceptor.
func (a *auth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		name, ok := a.actor(req.Header().Get("Authorization"))
		if !ok {
			return nil, errUnauthenticated
		}

		return next(context.WithValue(ctx, actorKey{}, name), req)
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (a *auth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (a *auth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		name, ok := a.actor(conn.RequestHeader().Get("Authorization"))
		if !ok {
			return errUnauthenticated
		}

		return next(context.WithValue(ctx, actorKey{}, name), conn)
	}
}
