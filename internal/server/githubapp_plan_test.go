package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

// forge records what godwit posted back, which is the only place the App's work is visible.
type forge struct {
	mu       sync.Mutex
	comments []string
	checks   []map[string]any
	approved bool
	srv      *httptest.Server
}

var migrationBody = map[string]string{
	"20260101000000_add_orders.up.sql":   "CREATE TABLE orders (id bigint PRIMARY KEY);\n",
	"20260101000000_add_orders.down.sql": "DROP TABLE orders;\n",
}

func newForge(t *testing.T) *forge {
	t.Helper()
	f := &forge{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *forge) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		_, _ = io.WriteString(w, `{"token":"ghs_x"}`)
	case strings.HasSuffix(r.URL.Path, "/permission"):
		_, _ = io.WriteString(w, `{"permission":"write"}`)
	case strings.HasSuffix(r.URL.Path, "/reviews"):
		_, _ = io.WriteString(w, f.reviews())
	case strings.HasSuffix(r.URL.Path, "/reactions"):
		_, _ = io.WriteString(w, `{}`)
	case strings.HasSuffix(r.URL.Path, "/files"):
		_, _ = io.WriteString(w, `[{"filename":"db/migrations/20260101000000_add_orders.up.sql"}]`)
	case strings.HasSuffix(r.URL.Path, "/contents/godwit.yaml"):
		_, _ = io.WriteString(w, `{"type":"file","size":40,"encoding":"base64","content":"`+
			base64.StdEncoding.EncodeToString([]byte("dir: db/migrations\ntarget: orders\n"))+`"}`)
	case strings.HasSuffix(r.URL.Path, "/contents/db/migrations"):
		_, _ = io.WriteString(w, `[{"name":"20260101000000_add_orders.up.sql","type":"file","size":44},
			{"name":"20260101000000_add_orders.down.sql","type":"file","size":19}]`)
	case strings.Contains(r.URL.Path, "/contents/db/migrations/"):
		_, _ = io.WriteString(w, migrationBody[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	case strings.HasSuffix(r.URL.Path, "/check-runs") || strings.Contains(r.URL.Path, "/check-runs/"):
		f.recordCheck(body)
		_, _ = io.WriteString(w, `{"id":991}`)
	case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost:
		f.recordComment(body)
		_, _ = io.WriteString(w, `{"id":1}`)
	case strings.HasSuffix(r.URL.Path, "/comments"):
		_, _ = io.WriteString(w, `[]`)
	case strings.Contains(r.URL.Path, "/pulls/"):
		_, _ = io.WriteString(w, `{"state":"open","changed_files":1,"user":{"login":"carol"},
			"head":{"sha":"`+webhookHead+`","repo":{"full_name":"`+webhookRepo+`"}}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *forge) reviews() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.approved {
		return `[]`
	}

	return `[{"state":"APPROVED","user":{"login":"bob"}}]`
}

func (f *forge) recordCheck(body []byte) {
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, out)
}

func (f *forge) recordComment(body []byte) {
	var out struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(body, &out)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, out.Body)
}

// concluded waits for the check the App closes when the command ends, which is the last thing it does.
func (f *forge) concluded(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		f.mu.Lock()
		for _, c := range f.checks {
			if c["status"] == "completed" {
				f.mu.Unlock()

				return c
			}
		}
		f.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("no check was concluded; checks = %v", f.checks)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (f *forge) posted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.comments...)
}

// An opened pull request reaches a plan on the real target and comes back as a report and a green
// check, with nothing in the repository but godwit.yaml.
func TestAPullRequestIsPlannedFromTheAppAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN, targetDSN := newDatabase(t, "ghplanstore"), newDatabase(t, "ghplantarget")
	gh := newForge(t)
	hook, apiURL := startWithWebhookAPI(t, storeDSN, GitHubApp{
		Addr: "127.0.0.1:0", Secret: webhookSecret, AppID: "1", PrivateKeyPEM: rsaPEM(t), APIBaseURL: gh.srv.URL,
	})
	if _, err := newClient(apiURL, "").RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "orders", Provider: "static", Dsn: targetDSN,
		GithubRepositories: &godwitv1.TargetRepositories{Values: []string{webhookRepo}},
	})); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"action":"opened","repository":{"id":42,"full_name":%q},"installation":{"id":7},
		"pull_request":{"number":3,"changed_files":1,
		"head":{"sha":%q,"repo":{"full_name":%q}}}}`, webhookRepo, webhookHead, webhookRepo)
	if resp := deliver(t, hook, "pull_request", "d-plan", body); resp.StatusCode != http.StatusAccepted {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, out)
	}

	check := gh.concluded(t)
	if check["conclusion"] != "success" {
		t.Fatalf("check = %v", check)
	}
	if out, _ := check["output"].(map[string]any); out["title"] != "1 to apply" {
		t.Fatalf("check output = %v", check["output"])
	}
	comments := gh.posted()
	if len(comments) != 1 {
		t.Fatalf("comments = %v", comments)
	}
	for _, want := range []string{"<!-- godwit:plan -->", "## godwit plan", "20260101000000_add_orders"} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("comment missing %q:\n%s", want, comments[0])
		}
	}
}
