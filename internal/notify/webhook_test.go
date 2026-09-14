package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	currentSecret  = "0123456789abcdef0123456789abcdef-current"
	previousSecret = "0123456789abcdef0123456789abcdef-previous"
	replayWindow   = 5 * time.Minute
)

func verifyDelivery(header string, body []byte, secret string, now time.Time) error {
	var timestamp string
	var signatures [][]byte
	for _, part := range strings.Split(header, ",") {
		key, value, _ := strings.Cut(part, "=")
		switch key {
		case "t":
			timestamp = value
		case "v1":
			if sig, err := hex.DecodeString(value); err == nil {
				signatures = append(signatures, sig)
			}
		}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("no timestamp")
	}
	if age := now.Sub(time.Unix(seconds, 0)); age > replayWindow || age < -replayWindow {
		return errors.New("timestamp outside the replay window")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	expected := mac.Sum(nil)
	for _, sig := range signatures {
		if hmac.Equal(sig, expected) {
			return nil
		}
	}

	return errors.New("no signature matches")
}

type delivery struct {
	header string
	body   []byte
}

func deliver(t *testing.T, secrets []string, sentAt time.Time, e Event) delivery {
	t.Helper()
	got := make(chan delivery, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- delivery{header: r.Header.Get(SignatureHeader), body: body}
	}))
	defer srv.Close()

	hook := Webhook{URL: srv.URL, Secrets: secrets, now: func() time.Time { return sentAt }}
	if err := hook.Notify(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	return <-got
}

type verifyCase struct {
	name   string
	body   func([]byte) []byte
	secret string
	now    time.Time
	ok     bool
}

func signatureCases(sentAt time.Time) []verifyCase {
	same := func(b []byte) []byte { return b }

	return []verifyCase{
		{"current secret", same, currentSecret, sentAt, true},
		{"previous secret during rotation", same, previousSecret, sentAt, true},
		{"wrong secret", same, "0123456789abcdef0123456789abcdef-attacker", sentAt, false},
		{"altered body", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"type":"detected"`, `"type":"resolved"`, 1))
		}, currentSecret, sentAt, false},
		{"inside the window", same, currentSecret, sentAt.Add(replayWindow), true},
		{"replayed after the window", same, currentSecret, sentAt.Add(replayWindow + time.Second), false},
		{"dated past the window", same, currentSecret, sentAt.Add(-replayWindow - time.Second), false},
	}
}

func TestWebhookSignsDelivery(t *testing.T) {
	t.Parallel()

	sentAt := time.Unix(1_788_000_000, 0)
	d := deliver(t, []string{currentSecret, previousSecret}, sentAt,
		Event{Kind: KindDrift, Type: DriftDetected, Target: "app", Detail: "+ column x"})

	var got map[string]string
	_ = json.Unmarshal(d.body, &got)
	if got["kind"] != "drift" || got["type"] != "detected" || got["target"] != "app" ||
		got["text"] != "godwit drift detected on app: + column x" {
		t.Fatalf("payload = %v", got)
	}
	parts := strings.Split(d.header, ",")
	if len(parts) != 3 || parts[0] != "t=1788000000" || !strings.HasPrefix(parts[1], "v1=") || !strings.HasPrefix(parts[2], "v1=") {
		t.Fatalf("header = %q", d.header)
	}
	for _, c := range signatureCases(sentAt) {
		err := verifyDelivery(d.header, c.body(d.body), c.secret, c.now)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	if err := verifyDelivery("v1=zz,v1="+strings.Repeat("0", 64), d.body, currentSecret, sentAt); err == nil {
		t.Fatal("a header without a timestamp must not verify")
	}
}

func TestWebhookSignsWithOneSecret(t *testing.T) {
	t.Parallel()

	sentAt := time.Unix(1_788_000_000, 0)
	d := deliver(t, []string{currentSecret}, sentAt, Event{Kind: KindRun, Type: RunSucceeded, Target: "app", RunID: "r1"})
	if strings.Count(d.header, "v1=") != 1 {
		t.Fatalf("header = %q", d.header)
	}
	if err := verifyDelivery(d.header, d.body, currentSecret, sentAt); err != nil {
		t.Fatal(err)
	}
	if err := verifyDelivery(d.header, d.body, previousSecret, sentAt); err == nil {
		t.Fatal("a secret godwit no longer signs with must not verify")
	}
}

func TestWebhookSignsAtSendTime(t *testing.T) {
	t.Parallel()

	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		header = r.Header.Get(SignatureHeader)
	}))
	defer srv.Close()

	before := time.Now().Unix()
	if err := (Webhook{URL: srv.URL, Secrets: []string{currentSecret}}).Notify(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
	ts, _ := strconv.ParseInt(strings.TrimPrefix(strings.Split(header, ",")[0], "t="), 10, 64)
	if ts < before || ts > time.Now().Unix() {
		t.Fatalf("header = %q, sent between %d and now", header, before)
	}
}

func TestWebhookErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if err := (Webhook{URL: srv.URL, Client: srv.Client()}).Notify(context.Background(), Event{}); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	if err := (Webhook{URL: "http://127.0.0.1:1"}).Notify(context.Background(), Event{}); err == nil ||
		!strings.Contains(err.Error(), "post webhook") {
		t.Fatalf("err = %v", err)
	}
	if err := (Webhook{URL: "://bad"}).Notify(context.Background(), Event{}); err == nil ||
		!strings.Contains(err.Error(), "build webhook request") {
		t.Fatalf("err = %v", err)
	}
}

func typescriptRecipe(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "run", "deployment.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(string(doc), "```typescript\n")
	recipe, _, closed := strings.Cut(rest, "```")
	if !found || !closed {
		t.Fatal("docs/run/deployment.md carries no typescript verification recipe")
	}

	return recipe
}

func TestTypeScriptRecipeVerifiesDelivery(t *testing.T) {
	t.Parallel()

	recipe := typescriptRecipe(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.ts")
	if err := os.WriteFile(probe, []byte("const probe: number = 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--experimental-strip-types", "--no-warnings", probe).CombinedOutput(); err != nil {
		t.Skipf("this node cannot run TypeScript: %s", out)
	}
	script := filepath.Join(dir, "verify.ts")
	harness := "\nconst [header, body, secret, now] = process.argv.slice(2);\n" +
		"process.exit(verifyGodwitSignature(header, Buffer.from(body, \"base64\"), secret, Number(now)) ? 0 : 1);\n"
	if err := os.WriteFile(script, []byte(recipe+harness), 0o600); err != nil {
		t.Fatal(err)
	}

	sentAt := time.Unix(1_788_000_000, 0)
	d := deliver(t, []string{currentSecret, previousSecret}, sentAt,
		Event{Kind: KindDrift, Type: DriftDetected, Target: "app", Detail: "+ column x"})
	for _, c := range signatureCases(sentAt) {
		cmd := exec.Command(node, "--experimental-strip-types", "--no-warnings", script, d.header,
			base64.StdEncoding.EncodeToString(c.body(d.body)), c.secret, strconv.FormatInt(c.now.Unix(), 10))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
			t.Fatalf("%s: node failed: %v\n%s", c.name, err, out)
		}
		if (err == nil) != c.ok {
			t.Errorf("%s: recipe verified = %v, want %v", c.name, err == nil, c.ok)
		}
	}
}
