package authz

import (
	"context"
	"reflect"
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestActorAndCallerOutsideARequest(t *testing.T) {
	t.Parallel()

	if Actor(context.Background()) != AnonymousActor || Caller(context.Background()) != (Principal{}) {
		t.Fatal("bare context must carry no scope and be named anonymous")
	}
	if err := Authorize(service+"ListRuns", Caller(context.Background())); err == nil {
		t.Fatal("a context without a principal must be refused")
	}
	p := Principal{Name: "ops", Scope: ScopeOperator}
	if Caller(WithPrincipal(context.Background(), p)) != p {
		t.Fatal("the principal must survive the context")
	}
}

func TestAuthorizeRefusesAnUnknownProcedure(t *testing.T) {
	t.Parallel()

	err := Authorize(service+"Unlisted", Principal{Name: "root", Scope: ScopeAdmin})
	if err == nil || err.Error() != "Unlisted requires scope ; token root has scope admin" {
		t.Fatalf("unknown procedure = %v, want a refusal even for admin", err)
	}
}

func TestParseTokens(t *testing.T) {
	t.Parallel()

	got, err := ParseTokens([]string{"s3", "bot:read:s4", "deploy:pipeline:s5", "ops:operator:s6", "root:admin:s7", "ci:admin:s8:with:colons"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Token{
		{Name: AnonymousActor, Scope: ScopeAdmin, Secret: "s3"},
		{Name: "bot", Scope: ScopeRead, Secret: "s4"},
		{Name: "deploy", Scope: ScopePipeline, Secret: "s5"},
		{Name: "ops", Scope: ScopeOperator, Secret: "s6"},
		{Name: "root", Scope: ScopeAdmin, Secret: "s7"},
		{Name: "ci", Scope: ScopeAdmin, Secret: "s8:with:colons"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens = %+v, want %+v", got, want)
	}
	for _, specs := range [][]string{{""}, {":s"}, {"ci:"}, {"ci:read:"}, {"ci:admin:same", "ops:admin:same"}, {"ci:root:same"}, {"ci::same"}} {
		_, err := ParseTokens(specs)
		if err == nil || strings.Contains(err.Error(), "same") {
			t.Fatalf("ParseTokens(%q) = %v, want an error without the secret", specs, err)
		}
	}
	if _, err := ParseTokens([]string{"ci:root:s"}); err == nil || !strings.Contains(err.Error(), `unknown scope "root"`) {
		t.Fatalf("unknown scope: %v", err)
	}
}

func TestParseTokensRefusesTwoFields(t *testing.T) {
	t.Parallel()

	_, err := ParseTokens([]string{"deploy:pipeline"})
	if err == nil || !strings.Contains(err.Error(), "two fields") || !strings.Contains(err.Error(), "name:scope:secret") {
		t.Fatalf("two-field spec = %v", err)
	}
	if strings.Contains(err.Error(), "pipeline") {
		t.Fatalf("the refusal must not echo the second field: %v", err)
	}
}

func TestScopeTableCoversEveryProcedure(t *testing.T) {
	t.Parallel()

	svc := godwitv1.File_godwit_v1_godwit_proto.Services().ByName("GodwitService")
	methods := svc.Methods()
	if methods.Len() != len(procedureScopes) {
		t.Fatalf("scope table has %d procedures, service has %d", len(procedureScopes), methods.Len())
	}
	for i := range methods.Len() {
		procedure := "/" + string(svc.FullName()) + "/" + string(methods.Get(i).Name())
		if _, ok := procedureScopes[procedure]; !ok {
			t.Fatalf("%s has no scope in the auth table", procedure)
		}
	}
}

func TestScopeAllows(t *testing.T) {
	t.Parallel()

	ordered := []Scope{ScopeRead, ScopePipeline, ScopeOperator, ScopeAdmin}
	for i, have := range ordered {
		for j, need := range ordered {
			if have.allows(need) != (i >= j) {
				t.Fatalf("%s.allows(%s) = %v", have, need, i >= j)
			}
		}
		if have.allows("") || have.allows("root") {
			t.Fatalf("%s must not allow an unknown scope", have)
		}
	}
}
