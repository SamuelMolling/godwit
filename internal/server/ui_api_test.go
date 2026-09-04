package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestUISignsInWithNamedTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st"), MasterKey: testKey, Holder: "r1",
		Tokens:    []string{"viewer:read:s-read", "sam:operator:s-op", "root:admin:s-admin"},
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
		UI: true,
	})
	client := newClient(baseURL, "s-admin")
	registerTarget(t, client, newDatabase(t, "tg"))
	runToSuccess(t, client, migrationFiles(), nil)

	web := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	call := func(method, path, secret string) (int, http.Header, string) {
		req, _ := http.NewRequestWithContext(ctx, method, baseURL+path, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if secret != "" {
			req.SetBasicAuth("whoever", secret)
		}
		resp, err := web.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)

		return resp.StatusCode, resp.Header, string(body)
	}

	if code, hdr, _ := call(http.MethodGet, "/ui/", ""); code != http.StatusUnauthorized || hdr.Get("WWW-Authenticate") != `Basic realm="godwit"` {
		t.Fatalf("tokens alone must protect the UI: code = %d headers = %v", code, hdr)
	}
	code, _, body := call(http.MethodGet, "/ui/drift", "s-read")
	if code != http.StatusOK || !strings.Contains(body, "viewer") || strings.Contains(body, `method="post"`) {
		t.Fatalf("read token: code = %d body = %s", code, body)
	}
	if code, _, body := call(http.MethodPost, "/ui/drift/app/accept", "s-read"); code != http.StatusForbidden ||
		!strings.Contains(body, "AcceptBaseline requires scope operator; token ui:viewer has scope read") {
		t.Fatalf("read token action: code = %d body = %s", code, body)
	}
	if code, hdr, _ := call(http.MethodPost, "/ui/drift/app/accept", "s-op"); code != http.StatusSeeOther ||
		hdr.Get("Location") != "/ui/drift?target=app" {
		t.Fatalf("operator token action: code = %d headers = %v", code, hdr)
	}

	audit, err := client.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{Target: "app", Limit: 1}))
	if err != nil || len(audit.Msg.Entries) != 1 || audit.Msg.Entries[0].Actor != "ui:sam" {
		t.Fatalf("audit = %+v, err = %v", audit, err)
	}

	cross, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/ui/drift/app/accept", nil)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	cross.SetBasicAuth("whoever", "s-op")
	resp, err := web.Do(cross)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	crossBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(crossBody), "cross-site request refused") ||
		resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("cross-site post: code = %d headers = %v body = %s", resp.StatusCode, resp.Header, crossBody)
	}
}

func TestUIScopeCapsTheSharedIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st"), MasterKey: testKey, Holder: "r1",
		Tokens:    []string{"root:admin:s-admin"},
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog,
		UI: true, UIUser: "sam", UIPassword: "pw", UIScope: "read",
	})
	registerTarget(t, newClient(baseURL, "s-admin"), newDatabase(t, "tg"))

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/ui/drift/app/check", nil)
	req.Header.Set("Origin", baseURL)
	req.SetBasicAuth("sam", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(string(body), "CheckDrift requires scope operator; token ui:sam has scope read") {
		t.Fatalf("code = %d body = %s", resp.StatusCode, body)
	}

	if err := Run(ctx, Config{UIScope: "boss", Log: testLog}); err == nil || !strings.Contains(err.Error(), "ui scope: unknown scope") {
		t.Fatalf("bad ui scope: %v", err)
	}
	if err := Run(ctx, Config{UIOrigins: []string{"godwit.example.com"}, Log: testLog}); err == nil ||
		!strings.Contains(err.Error(), `ui origin "godwit.example.com"`) {
		t.Fatalf("bad ui origin: %v", err)
	}
}

func TestUIDiffSuppliesTheRepeatableFilesItStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st") + "&search_path=public", MasterKey: testKey, Holder: "r1",
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond}, Log: testLog, UI: true,
	})
	client := newClient(baseURL, "")
	registerTarget(t, client, newDatabase(t, "tg")+"&search_path=public")
	runToSuccess(t, client, append(orderedFiles()[:4:4],
		&godwitv1.MigrationFile{Name: "R__t_totals.up.sql", Body: "CREATE OR REPLACE VIEW t_totals AS SELECT id FROM t;"},
		&godwitv1.MigrationFile{Name: "R__t_totals.down.sql", Body: "DROP VIEW IF EXISTS t_totals;"}), nil)

	post := func(form url.Values) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/ui/diff", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", baseURL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)

		return resp.StatusCode, string(body)
	}

	code, body := post(url.Values{"target": {"app"}, "schema": {"CREATE TABLE t (id int, a int);"}})
	if code != http.StatusOK || strings.Contains(body, "DROP VIEW") || !strings.Contains(body, "No changes") {
		t.Fatalf("code = %d body = %s", code, body)
	}
	if !strings.Contains(body, "Supplied from run ") || !strings.Contains(body, "matches the snapshot, byte for byte") {
		t.Fatalf("provenance missing: %s", body)
	}

	code, body = post(url.Values{
		"target": {"app"}, "schema": {"CREATE TABLE t (id int, a int);"},
		"files": {"paste"}, "body.t_totals": {"CREATE OR REPLACE VIEW t_totals AS SELECT id, a FROM t;"},
	})
	if code != http.StatusOK || !strings.Contains(body, "differs from what app recorded") ||
		!strings.Contains(body, "rebuilds it from the body you pasted") || !strings.Contains(body, "CREATE VIEW") {
		t.Fatalf("pasted body: code = %d body = %s", code, body)
	}
}
