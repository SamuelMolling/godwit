package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

// applied waits for the comment the reporter posts once the run has settled, which is the last thing
// that happens on a pull request that commanded an apply.
func (f *forge) applied(t *testing.T) string {
	t.Helper()
	deadline := time.After(90 * time.Second)
	for {
		for _, c := range f.posted() {
			if strings.Contains(c, "<!-- godwit:migrate -->") && strings.Contains(c, "applied to") {
				return c
			}
		}
		select {
		case <-deadline:
			t.Fatalf("the run was never reported; comments = %v", f.posted())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (f *forge) reviewed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = true
}

// A `godwit apply` comment reaches the database and comes back as the run's own report, on a pull
// request holding nothing but godwit.yaml.
func TestAnApplyCommentReachesTheDatabaseAndReportsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN, targetDSN := newDatabase(t, "ghapplystore"), newDatabase(t, "ghapplytarget")
	gh := newForge(t)
	gh.reviewed()
	hook, apiURL := startWithWebhookAPI(t, storeDSN, GitHubApp{
		Addr: "127.0.0.1:0", Secret: webhookSecret, AppID: "1", PrivateKeyPEM: rsaPEM(t), APIBaseURL: gh.srv.URL,
	})
	if _, err := newClient(apiURL, "").RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "orders", Provider: "static", Dsn: targetDSN,
		GithubRepositories: &godwitv1.TargetRepositories{Values: []string{webhookRepo}},
	})); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"action":"created","repository":{"id":42,"full_name":%q},"installation":{"id":7},
		"issue":{"number":3,"pull_request":{"url":"x"}},
		"comment":{"body":"godwit apply","created_at":%q,"user":{"login":"alice"},"author_association":"MEMBER"}}`,
		webhookRepo, time.Now().UTC().Format(time.RFC3339))
	if resp := deliver(t, hook, "issue_comment", "d-apply", body); resp.StatusCode != http.StatusAccepted {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, out)
	}

	report := gh.applied(t)
	if !strings.Contains(report, "1 migration applied to `orders`") {
		t.Fatalf("report = %s", report)
	}
	// The migration is on the database, not merely reported as such.
	if got := scalar[int64](t, targetDSN, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'orders'`); got != 1 {
		t.Fatalf("the orders table is not there (%d)", got)
	}
	if got := scalar[int64](t, targetDSN, `SELECT count(*) FROM godwit.migrations`); got != 1 {
		t.Fatalf("the journal carries %d migrations", got)
	}
	concluded := gh.concluded(t)
	if concluded["conclusion"] != "success" {
		t.Fatalf("check = %v", concluded)
	}
}
