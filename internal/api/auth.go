package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

// AnonymousActor names calls made with an unnamed token or against a service without tokens.
const AnonymousActor = "anonymous"

// Scope is what a token may call; each scope includes everything below it.
type Scope string

// Scopes from least to most privileged.
const (
	ScopeRead     Scope = "read"
	ScopePipeline Scope = "pipeline"
	ScopeOperator Scope = "operator"
	ScopeAdmin    Scope = "admin"
)

var scopeRank = map[Scope]int{ScopeRead: 1, ScopePipeline: 2, ScopeOperator: 3, ScopeAdmin: 4}

func (s Scope) allows(required Scope) bool {
	r, ok := scopeRank[required]

	return ok && scopeRank[s] >= r
}

// ParseScope returns s as a Scope, refusing anything outside the known set.
func ParseScope(s string) (Scope, error) {
	if _, ok := scopeRank[Scope(s)]; !ok {
		return "", fmt.Errorf("unknown scope %q, want read, pipeline, operator or admin", s)
	}

	return Scope(s), nil
}

var procedureScopes = map[string]Scope{
	godwitv1connect.GodwitServiceGetRunProcedure:          ScopeRead,
	godwitv1connect.GodwitServiceListRunsProcedure:        ScopeRead,
	godwitv1connect.GodwitServiceWatchRunProcedure:        ScopeRead,
	godwitv1connect.GodwitServicePlanRunProcedure:         ScopeRead,
	godwitv1connect.GodwitServiceGetTargetStatusProcedure: ScopeRead,
	godwitv1connect.GodwitServiceListTargetsProcedure:     ScopeRead,
	godwitv1connect.GodwitServiceListDriftEventsProcedure: ScopeRead,
	godwitv1connect.GodwitServiceListAuditProcedure:       ScopeRead,
	godwitv1connect.GodwitServiceGetPlanProcedure:         ScopeRead,
	godwitv1connect.GodwitServiceListPlansProcedure:       ScopeRead,
	godwitv1connect.GodwitServiceDiffProcedure:            ScopeRead,
	godwitv1connect.GodwitServiceCreateRunProcedure:       ScopePipeline,
	godwitv1connect.GodwitServiceRevertRunProcedure:       ScopePipeline,
	godwitv1connect.GodwitServiceConfirmRolloutProcedure:  ScopePipeline,
	godwitv1connect.GodwitServiceResumeRunProcedure:       ScopeOperator,
	godwitv1connect.GodwitServiceParkRunProcedure:         ScopeOperator,
	godwitv1connect.GodwitServiceCheckDriftProcedure:      ScopeOperator,
	godwitv1connect.GodwitServiceAcceptBaselineProcedure:  ScopeOperator,
	godwitv1connect.GodwitServiceBaselineTargetProcedure:  ScopeOperator,
	godwitv1connect.GodwitServiceRegisterTargetProcedure:  ScopeAdmin,
}

// Token is one accepted bearer secret with the actor name and scope it resolves to.
type Token struct {
	Name   string
	Scope  Scope
	Secret string
}

// ParseTokens reads token specs of the form "name:scope:secret"; "name:secret" is admin and a bare "secret" is an anonymous admin.
func ParseTokens(specs []string) ([]Token, error) {
	seen := map[string]string{}
	out := make([]Token, 0, len(specs))
	for i, spec := range specs {
		parts := strings.SplitN(strings.TrimSpace(spec), ":", 3)
		t := Token{Name: AnonymousActor, Scope: ScopeAdmin, Secret: parts[len(parts)-1]}
		if len(parts) > 1 {
			t.Name = parts[0]
		}
		if len(parts) == 3 {
			t.Scope = Scope(parts[1])
		}
		if t.Name == "" || t.Secret == "" {
			return nil, fmt.Errorf("token #%d: want name:scope:secret, name:secret or a bare secret", i+1)
		}
		if _, err := ParseScope(string(t.Scope)); err != nil {
			return nil, fmt.Errorf("token #%d (%s): %w", i+1, t.Name, err)
		}
		if other, dup := seen[t.Secret]; dup {
			return nil, fmt.Errorf("token #%d (%s): secret already used by %s", i+1, t.Name, other)
		}
		seen[t.Secret] = t.Name
		out = append(out, t)
	}

	return out, nil
}

// Principal is the identity behind a call: the token name and its scope.
type Principal struct {
	Name  string
	Scope Scope
}

type principalKey struct{}

var anonymousAdmin = Principal{Name: AnonymousActor, Scope: ScopeAdmin}

// Caller returns the principal behind the call; outside an authenticated request it is an anonymous admin.
func Caller(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}

	return anonymousAdmin
}

// WithPrincipal returns ctx carrying p as the caller, the way the auth interceptor does for a bearer token.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Actor returns the name of the token behind the call, or anonymous outside an authenticated request.
func Actor(ctx context.Context) string {
	return Caller(ctx).Name
}

// auth checks bearer tokens against the allow-set, names the caller and enforces the per-procedure scope; an empty set disables auth.
type auth struct {
	principals map[string]Principal
}

func newAuth(tokens []Token) *auth {
	principals := map[string]Principal{}
	for _, t := range tokens {
		principals[t.Secret] = Principal{Name: t.Name, Scope: t.Scope}
	}

	return &auth{principals: principals}
}

var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or missing bearer token"))

// actor resolves the Authorization header to a principal; ok is false when the call must be refused.
func (a *auth) actor(header string) (Principal, bool) {
	if len(a.principals) == 0 {
		return anonymousAdmin, true
	}
	secret, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return Principal{}, false
	}
	p, ok := a.principals[secret]

	return p, ok
}

// Authorize is the scope decision the interceptor makes, for callers that reach a handler without one.
func Authorize(procedure string, p Principal) error {
	required := procedureScopes[procedure]
	if p.Scope.allows(required) {
		return nil
	}
	method := procedure[strings.LastIndex(procedure, "/")+1:]

	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("%s requires scope %s; token %s has scope %s", method, required, p.Name, p.Scope))
}

func (a *auth) authorize(ctx context.Context, procedure, header string) (context.Context, error) {
	p, ok := a.actor(header)
	if !ok {
		return ctx, errUnauthenticated
	}
	if err := Authorize(procedure, p); err != nil {
		return ctx, err
	}

	return WithPrincipal(ctx, p), nil
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
