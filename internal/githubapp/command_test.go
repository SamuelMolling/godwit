package githubapp

import (
	"context"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
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

	store := newStore(nil)
	cmd := Command{
		Delivery: "d1", Event: EventIssueComment, Repository: testRepo, Number: 3, Head: testHead,
		Login: "alice", Name: "apply", Bindings: Bindings{{Target: "orders"}},
		Principal: api.Principal{Name: "github:" + testRepo, Scope: api.ScopePipeline},
	}
	if err := (Recorder{Log: testLog}).Enqueue(context.Background(), store, cmd); err != nil {
		t.Fatal(err)
	}
	if len(store.audits) != 1 {
		t.Fatalf("audits = %v", store.audits)
	}
	e := store.audits[0]
	if e.Actor != "github:"+testRepo || e.Action != controlplane.AuditWebhookCommand {
		t.Fatalf("entry = %+v", e)
	}
	for _, want := range []string{"command=apply", "login=alice", "delivery=d1", "head=" + testHead, "bound=orders"} {
		if !strings.Contains(e.Detail, want) {
			t.Fatalf("detail %q does not carry %q", e.Detail, want)
		}
	}
}

func TestAPlanCarriesNoCommandingLogin(t *testing.T) {
	t.Parallel()

	store := newStore(nil)
	cmd := Command{Delivery: "d2", Name: "plan", Principal: api.Principal{Scope: api.ScopeRead}}
	if err := (Recorder{Log: testLog}).Enqueue(context.Background(), store, cmd); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(store.audits[0].Detail, "login=") {
		t.Fatalf("detail = %q", store.audits[0].Detail)
	}
}

func TestRecorderPassesTheStoreFailureUp(t *testing.T) {
	t.Parallel()

	store := newStore(nil)
	store.auditErr = errBroken
	if err := (Recorder{Log: testLog}).Enqueue(context.Background(), store, Command{}); err == nil {
		t.Fatal("no error")
	}
}
