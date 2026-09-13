package githubapp

import (
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/authz"
)

func TestAWebhookCanNeverReachOperatorOrAdmin(t *testing.T) {
	t.Parallel()

	for name, scope := range scopes {
		if scope != authz.ScopeRead && scope != authz.ScopePipeline {
			t.Fatalf("%s resolves to scope %s", name, scope)
		}
	}
	for _, name := range []string{"resume", "park", "baseline", "target add", ""} {
		if scope, ok := scopes[name]; ok {
			t.Fatalf("%s is reachable from a webhook at scope %s", name, scope)
		}
	}
}

func TestAuditEntrySaysWhichDeliveryAndWhatItReached(t *testing.T) {
	t.Parallel()

	cmd := command{
		delivery: "d1", event: eventIssueComment, repository: testRepo, number: 3, head: testHead,
		name: "apply", bound: bindings{{target: "orders"}},
		principal: authz.Principal{Name: forgeActor(testRepo, "alice"), Scope: authz.ScopePipeline},
	}
	for _, want := range []string{
		"command=apply", "delivery=d1", "head=" + testHead, "bound=orders",
		"scope=pipeline", "event=issue_comment", "pr=3",
	} {
		if !strings.Contains(cmd.detail(), want) {
			t.Fatalf("detail %q does not carry %q", cmd.detail(), want)
		}
	}
	if strings.Contains(cmd.detail(), "login=") {
		t.Fatalf("the commander is the actor, not a detail field: %q", cmd.detail())
	}
}

func TestAForgeActorNamesTheRepositoryThenWhoAskedIt(t *testing.T) {
	t.Parallel()

	if got := forgeActor(testRepo, "alice"); got != "github:"+testRepo+":alice" {
		t.Fatalf("forgeActor = %q", got)
	}
	for _, marker := range []string{autoplanActor, reportActor} {
		if validLogin(marker) {
			t.Fatalf("%s is spelled like a github login and could collide with a person", marker)
		}
	}
}
