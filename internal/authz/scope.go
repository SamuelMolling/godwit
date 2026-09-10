// Package authz decides who may command godwit, over the API and through the forge, and serves no transport.
package authz

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
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

// Spelled out rather than imported from godwitv1connect, which would put transport in a policy; scope_test.go holds the table to the service descriptor.
const service = "/godwit.v1.GodwitService/"

var procedureScopes = map[string]Scope{
	service + "GetRun":                  ScopeRead,
	service + "ListRuns":                ScopeRead,
	service + "WatchRun":                ScopeRead,
	service + "PlanRun":                 ScopeRead,
	service + "GetTargetStatus":         ScopeRead,
	service + "ListTargets":             ScopeRead,
	service + "ListMigrations":          ScopeRead,
	service + "ListDriftEvents":         ScopeRead,
	service + "ListAudit":               ScopeRead,
	service + "GetPlan":                 ScopeRead,
	service + "ListPlans":               ScopeRead,
	service + "Diff":                    ScopeRead,
	service + "Checkpoint":              ScopeRead,
	service + "ListCredentialStores":    ScopeRead,
	service + "CreateRun":               ScopePipeline,
	service + "RevertRun":               ScopePipeline,
	service + "ConfirmRollout":          ScopePipeline,
	service + "ResumeRun":               ScopeOperator,
	service + "ParkRun":                 ScopeOperator,
	service + "CheckDrift":              ScopeOperator,
	service + "AcceptBaseline":          ScopeOperator,
	service + "BaselineTarget":          ScopeOperator,
	service + "ReconcileTarget":         ScopeOperator,
	service + "RegisterTarget":          ScopeAdmin,
	service + "RegisterCredentialStore": ScopeAdmin,
}

// Token is one accepted bearer secret with the actor name and scope it resolves to.
type Token struct {
	Name   string
	Scope  Scope
	Secret string
}

// ParseTokens reads token specs of the form "name:scope:secret"; a bare "secret" is an anonymous admin.
func ParseTokens(specs []string) ([]Token, error) {
	seen := map[string]string{}
	out := make([]Token, 0, len(specs))
	for i, spec := range specs {
		t, err := parseToken(strings.TrimSpace(spec))
		if err != nil {
			return nil, fmt.Errorf("token #%d: %w", i+1, err)
		}
		if other, dup := seen[t.Secret]; dup {
			return nil, fmt.Errorf("token #%d (%s): secret already used by %s", i+1, t.Name, other)
		}
		seen[t.Secret] = t.Name
		out = append(out, t)
	}

	return out, nil
}

var errTokenForm = errors.New("want name:scope:secret or a bare secret")

func parseToken(spec string) (Token, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) == 2 {
		return Token{}, fmt.Errorf("%q has two fields; that form used to read the second one as the secret and grant admin: %w",
			parts[0]+":…", errTokenForm)
	}
	if len(parts) == 1 {
		if parts[0] == "" {
			return Token{}, errTokenForm
		}

		return Token{Name: AnonymousActor, Scope: ScopeAdmin, Secret: parts[0]}, nil
	}
	t := Token{Name: parts[0], Scope: Scope(parts[1]), Secret: parts[2]}
	if t.Name == "" || t.Secret == "" {
		return Token{}, errTokenForm
	}
	if _, err := ParseScope(string(t.Scope)); err != nil {
		return Token{}, fmt.Errorf("(%s): %w", t.Name, err)
	}

	return t, nil
}

// Principal is the identity behind a call: the token name and its scope.
type Principal struct {
	Name  string
	Scope Scope
}

type principalKey struct{}

// Caller returns the principal behind the call; a context that never passed the interceptor carries none, and Authorize refuses those.
func Caller(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)

	return p
}

// WithPrincipal returns ctx carrying p as the caller, the way the auth interceptor does for a bearer token.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Actor returns the name of the token behind the call, or anonymous outside an authenticated request.
func Actor(ctx context.Context) string {
	return cmp.Or(Caller(ctx).Name, AnonymousActor)
}

// Authorize reports whether p reaches procedure, refusing an unknown one; the error is plain because the wire code is the caller's to choose.
func Authorize(procedure string, p Principal) error {
	required := procedureScopes[procedure]
	if p.Scope.allows(required) {
		return nil
	}
	method := procedure[strings.LastIndex(procedure, "/")+1:]

	return fmt.Errorf("%s requires scope %s; token %s has scope %s", method, required, p.Name, p.Scope)
}
