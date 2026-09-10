package server

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/controlplane"
)

const (
	webhookPath   = "/github/webhook"
	webhookSecret = "s3cret"
	webhookRepo   = "acme/orders"
	webhookHead   = "1111111111111111111111111111111111111111"
)

func rsaPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = io.WriteString(w, `{"token":"ghs_x"}`)
		case strings.HasSuffix(r.URL.Path, "/collaborators/alice/permission"):
			_, _ = io.WriteString(w, `{"permission":"write"}`)
		case strings.HasSuffix(r.URL.Path, "/collaborators/bob/permission"):
			_, _ = io.WriteString(w, `{"permission":"admin"}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = io.WriteString(w, `[{"filename":"db/migrations/20260101000000_a.up.sql"}]`)
		case strings.Contains(r.URL.Path, "/contents/godwit.yaml"):
			_, _ = io.WriteString(w, `{"type":"file","size":40,"encoding":"base64","content":"ZGlyOiBkYi9taWdyYXRpb25zCnRhcmdldDogb3JkZXJzCg=="}`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = io.WriteString(w, `[{"state":"APPROVED","commit_id":"`+webhookHead+`","user":{"login":"bob"}}]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = io.WriteString(w, `{"state":"open","user":{"login":"carol"},
				"head":{"sha":"`+webhookHead+`","repo":{"full_name":"`+webhookRepo+`"}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func startWithWebhook(t *testing.T, storeDSN string, app GitHubApp) string {
	t.Helper()
	hook, _ := startWithWebhookAPI(t, storeDSN, app)

	return hook
}

func startWithWebhookAPI(t *testing.T, storeDSN string, app GitHubApp) (hook, apiURL string) {
	t.Helper()
	ready := make(chan net.Addr, 1)
	app.OnReady = func(addr net.Addr) { ready <- addr }
	apiURL = startServiceCfg(t, Config{
		Listen:    "127.0.0.1:0",
		StoreDSN:  storeDSN,
		Keys:      testKeys,
		Holder:    "webhook",
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond},
		Log:       testLog,
		GitHub:    app,
	})
	select {
	case addr := <-ready:
		return "http://" + addr.String() + webhookPath, apiURL
	case <-time.After(15 * time.Second):
		t.Fatal("the webhook listener never came up")
	}

	return "", ""
}

func deliver(t *testing.T, url, event, delivery, body string) *http.Response {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func storeAt(t *testing.T, dsn string) *controlplane.Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	return controlplane.NewStore(pool)
}

func TestWebhookIsItsOwnListener(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "webhook")
	gh := fakeGitHub(t)
	url := startWithWebhook(t, storeDSN, GitHubApp{
		Addr: "127.0.0.1:0", Secret: webhookSecret, AppID: "1", PrivateKeyPEM: rsaPEM(t), APIBaseURL: gh.URL,
	})
	execStore(t, storeDSN, `INSERT INTO cp_targets (name, provider, config)
		VALUES ('orders', 'static', '{"github_repositories":"`+webhookRepo+`"}')`)

	body := fmt.Sprintf(`{"action":"created","repository":{"id":42,"full_name":%q},"installation":{"id":7},
		"issue":{"number":3,"pull_request":{"url":"x"}},
		"comment":{"body":"godwit apply","created_at":%q,"user":{"login":"alice"},"author_association":"MEMBER"}}`,
		webhookRepo, time.Now().UTC().Format(time.RFC3339))

	resp := deliver(t, url, "issue_comment", "d1", body)
	if resp.StatusCode != http.StatusAccepted {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, out)
	}
	if again := deliver(t, url, "issue_comment", "d1", body); again.StatusCode != http.StatusAccepted {
		t.Fatalf("redelivery status = %d", again.StatusCode)
	}

	entries, err := storeAt(t, storeDSN).ListAudit(ctx, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var webhookEntries []controlplane.AuditEntry
	for _, e := range entries {
		if e.Action == controlplane.AuditWebhookCommand {
			webhookEntries = append(webhookEntries, e)
		}
	}
	if len(webhookEntries) != 1 {
		t.Fatalf("audit = %+v, want exactly one webhook entry after a delivery and its replay", webhookEntries)
	}
	e := webhookEntries[0]
	if e.Actor != "github:"+webhookRepo {
		t.Fatalf("actor = %q, want the installation, not the person", e.Actor)
	}
	for _, want := range []string{"login=alice", "delivery=d1", "command=apply"} {
		if !strings.Contains(e.Detail, want) {
			t.Fatalf("detail = %q, want it to carry %q", e.Detail, want)
		}
	}
}

func TestWebhookListenerIsOffByDefault(t *testing.T) {
	t.Parallel()

	url := startService(t, newDatabase(t, "nowebhook"), "r1", nil)
	resp, err := http.Get(url + webhookPath) //nolint:noctx // a one-line probe of a listener under test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("the api listener answered the webhook path")
	}
}

func TestWebhookConfigurationFailsStartUp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		app  GitHubApp
		want string
	}{
		{"no secret", GitHubApp{Addr: "127.0.0.1:0", AppID: "1", PrivateKeyPEM: rsaPEM(t)}, "needs a webhook secret"},
		{"no app id", GitHubApp{Addr: "127.0.0.1:0", Secret: "s", PrivateKeyPEM: rsaPEM(t)}, "needs a github app id"},
		{"no private key", GitHubApp{Addr: "127.0.0.1:0", Secret: "s", AppID: "1"}, "is not PEM"},
		{
			"a private key that is not one",
			GitHubApp{Addr: "127.0.0.1:0", Secret: "s", AppID: "1", PrivateKeyPEM: "-----BEGIN X-----\nAAAA\n-----END X-----\n"},
			"private key",
		},
		{
			"an association that is not access",
			GitHubApp{
				Addr: "127.0.0.1:0", Secret: "s", AppID: "1", PrivateKeyPEM: rsaPEM(t),
				Associations: []string{"NONE"},
			},
			"NONE is not access",
		},
		{
			"an address that cannot be bound",
			GitHubApp{Addr: "256.0.0.1:0", Secret: "s", AppID: "1", PrivateKeyPEM: rsaPEM(t)},
			"lookup",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Run(context.Background(), Config{
				Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "badhook"), Keys: testKeys,
				StoreMaxConns: 4, Log: testLog, GitHub: tc.app,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want it to carry %q", err, tc.want)
			}
		})
	}
}

func TestPrivateKeyFormats(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := parsePrivateKey(pkcs8); err != nil {
		t.Fatalf("pkcs8: %v", err)
	}
	if _, err := parsePrivateKey(rsaPEM(t)); err != nil {
		t.Fatalf("pkcs1: %v", err)
	}
	agreement, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err = x509.MarshalPKCS8PrivateKey(agreement)
	if err != nil {
		t.Fatal(err)
	}
	notASigner := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := parsePrivateKey(notASigner); err == nil || !strings.Contains(err.Error(), "cannot sign") {
		t.Fatalf("err = %v", err)
	}
}
