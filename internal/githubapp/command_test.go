package githubapp

import (
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/api"
)

func TestAWebhookCanNeverReachOperatorOrAdmin(t *testing.T) {
	t.Parallel()

	for name, scope := range scopes {
		if scope != api.ScopeRead && scope != api.ScopePipeline {
			t.Fatalf("%s resolves to scope %s", name, scope)
		}
	}
	for _, name := range []string{"resume", "park", "baseline", "target add", ""} {
		if scope, ok := scopes[name]; ok {
			t.Fatalf("%s is reachable from a webhook at scope %s", name, scope)
		}
	}
}

func TestAuditEntrySaysWhoAskedAndWhichDelivery(t *testing.T) {
	t.Parallel()

	cmd := command{
		delivery: "d1", event: eventIssueComment, repository: testRepo, number: 3, head: testHead,
		login: "alice", name: "apply", bound: bindings{{target: "orders"}},
		principal: api.Principal{Name: "github:" + testRepo, Scope: api.ScopePipeline},
	}
	for _, want := range []string{
		"command=apply", "login=alice", "delivery=d1", "head=" + testHead, "bound=orders",
		"scope=pipeline", "event=issue_comment", "pr=3",
	} {
		if !strings.Contains(cmd.detail(), want) {
			t.Fatalf("detail %q does not carry %q", cmd.detail(), want)
		}
	}
}

func TestAPlanCarriesNoCommandingLogin(t *testing.T) {
	t.Parallel()

	cmd := command{delivery: "d2", name: "plan", principal: api.Principal{Scope: api.ScopeRead}}
	if strings.Contains(cmd.detail(), "login=") {
		t.Fatalf("detail = %q", cmd.detail())
	}
}
