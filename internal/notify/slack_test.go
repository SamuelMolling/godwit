package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type slackCall struct {
	Method  string
	Payload map[string]any
}

type fakeSlack struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []slackCall
	scripts []func(w http.ResponseWriter) bool
	seq     int
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeSlack) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer xoxb-test" {
		f.t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
	}
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		f.t.Errorf("decode payload: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/")
	f.calls = append(f.calls, slackCall{Method: method, Payload: payload})
	if len(f.scripts) > 0 {
		script := f.scripts[0]
		f.scripts = f.scripts[1:]
		if script(w) {
			return
		}
	}
	f.seq++
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "channel": "C1", "ts": fmt.Sprintf("1700000000.%06d", f.seq),
	})
}

func (f *fakeSlack) script(fns ...func(w http.ResponseWriter) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, fns...)
}

func (f *fakeSlack) recorded() []slackCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]slackCall(nil), f.calls...)
}

func status(code int, header ...string) func(w http.ResponseWriter) bool {
	return func(w http.ResponseWriter) bool {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(code)

		return true
	}
}

func body(s string) func(w http.ResponseWriter) bool {
	return func(w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(s))

		return true
	}
}

type memTS struct {
	mu   sync.Mutex
	rows map[string][2]string
	err  error
}

func (m *memTS) GetTS(_ context.Context, kind, key string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[kind+"/"+key]

	return row[0], row[1], m.err
}

func (m *memTS) PutTS(_ context.Context, kind, key, channel, ts string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string][2]string{}
	}
	m.rows[kind+"/"+key] = [2]string{channel, ts}

	return m.err
}

func noSleep(context.Context, time.Duration) error { return nil }

func newSlack(f *fakeSlack, store TSStore, mode string) Slack {
	return Slack{
		Token: "xoxb-test", Channel: "#ops", Mode: mode, Client: f.srv.Client(),
		Store: store, BaseURL: f.srv.URL, PublicURL: "https://godwit.example.com/", Sleep: noSleep,
	}
}

func runEvent(typ, state string) Event {
	return Event{
		Kind: KindRun, Type: typ, Target: "app", RunID: "12345678-0000-0000-0000-000000000000",
		State: state, Attempt: 1, Rollout: "direct", Phase: "expand", At: time.Unix(0, 0),
	}
}

func TestSlackThreadMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeSlack(t)
	store := &memTS{}
	s := newSlack(f, store, ModeThread)

	if err := s.Notify(ctx, runEvent(RunCreated, "queued")); err != nil {
		t.Fatal(err)
	}
	if err := s.Notify(ctx, Event{Kind: KindRun, Type: RunFailed, Target: "app", RunID: "12345678-0000-0000-0000-000000000000", State: "failed", Detail: "boom"}); err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if len(calls) != 3 || calls[0].Method != "chat.postMessage" || calls[1].Method != "chat.postMessage" || calls[2].Method != "chat.update" {
		t.Fatalf("calls = %+v", calls)
	}
	root := calls[0].Payload
	if root["channel"] != "#ops" || root["thread_ts"] != nil || root["text"] != "godwit run created on app (run 12345678)" {
		t.Fatalf("root = %v", root)
	}
	blocks := root["blocks"].([]any)
	if len(blocks) != 3 || blocks[0].(map[string]any)["type"] != "header" || blocks[2].(map[string]any)["type"] != "actions" {
		t.Fatalf("root blocks = %v", blocks)
	}
	button := blocks[2].(map[string]any)["elements"].([]any)[0].(map[string]any)
	if button["url"] != "https://godwit.example.com/ui/runs/12345678-0000-0000-0000-000000000000" {
		t.Fatalf("button = %v", button)
	}
	fields := blocks[1].(map[string]any)["fields"].([]any)
	if len(fields) != 4 || !strings.Contains(fields[0].(map[string]any)["text"].(string), "🕐 queued") ||
		!strings.Contains(fields[1].(map[string]any)["text"].(string), "direct / expand") {
		t.Fatalf("fields = %v", fields)
	}
	reply := calls[1].Payload
	if reply["channel"] != "C1" || reply["thread_ts"] != "1700000000.000001" {
		t.Fatalf("reply = %v", reply)
	}
	replyText := reply["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	if replyText != "❌ failed · *failed*\n```boom```" {
		t.Fatalf("reply text = %q", replyText)
	}
	update := calls[2].Payload
	if update["ts"] != "1700000000.000001" || len(update["blocks"].([]any)) != 4 {
		t.Fatalf("update = %v", update)
	}
	if ch, ts, _ := store.GetTS(ctx, KindRun, "12345678-0000-0000-0000-000000000000"); ch != "C1" || ts != "1700000000.000001" {
		t.Fatalf("stored = %s %s", ch, ts)
	}
}

func TestSlackEditModeAndDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeSlack(t)
	s := newSlack(f, &memTS{}, ModeEdit)
	s.PublicURL = ""

	e := runEvent(RunCreated, "queued")
	e.Attempt, e.Rollout = 0, ""
	if err := s.Notify(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.Notify(ctx, runEvent(RunRunning, "running")); err != nil {
		t.Fatal(err)
	}
	drift := Event{Kind: KindDrift, Type: DriftDetected, Target: "app", Detail: strings.Repeat("x", 600), At: time.Unix(0, 0)}
	for _, typ := range []string{DriftDetected, DriftResolved, DriftDetected, DriftAccepted} {
		drift.Type = typ
		if err := s.Notify(ctx, drift); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.recorded()
	methods := make([]string, len(calls))
	for i, c := range calls {
		methods[i] = c.Method
	}
	want := "chat.postMessage chat.update chat.postMessage chat.update chat.postMessage chat.update"
	if strings.Join(methods, " ") != want {
		t.Fatalf("methods = %v", methods)
	}
	first := calls[0].Payload["blocks"].([]any)
	if len(first) != 2 || len(first[1].(map[string]any)["fields"].([]any)) != 2 {
		t.Fatalf("minimal blocks = %v", first)
	}
	detected := calls[2].Payload
	header := detected["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"]
	code := detected["blocks"].([]any)[2].(map[string]any)["text"].(map[string]any)["text"].(string)
	if header != "godwit · app · drift" || !strings.HasSuffix(code, "…```") || len([]rune(code)) != slackDetailLimit+7 {
		t.Fatalf("detected = %v %q", header, code)
	}
	if calls[3].Payload["ts"] != "1700000000.000003" || calls[5].Payload["ts"] != "1700000000.000005" {
		t.Fatalf("drift updates = %v / %v", calls[3].Payload, calls[5].Payload)
	}
	resolved := calls[3].Payload["blocks"].([]any)[1].(map[string]any)["fields"].([]any)[0].(map[string]any)["text"]
	if resolved != "*State*\n✅ drift resolved" {
		t.Fatalf("resolved badge = %v", resolved)
	}
}

func TestSlackRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeSlack(t)
	var waits []time.Duration
	s := newSlack(f, &memTS{}, ModeThread)
	s.Sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)

		return nil
	}

	f.script(status(http.StatusInternalServerError), status(http.StatusTooManyRequests, "Retry-After", "7"), status(http.StatusBadGateway))
	if err := s.Notify(ctx, runEvent(RunCreated, "queued")); err != nil {
		t.Fatal(err)
	}
	if len(f.recorded()) != 4 || fmt.Sprint(waits) != "[1s 7s 4s]" {
		t.Fatalf("calls = %d, waits = %v", len(f.recorded()), waits)
	}

	f.script(status(500), status(500), status(500), status(500))
	err := s.Notify(ctx, runEvent(RunRunning, "running"))
	if err == nil || !strings.Contains(err.Error(), "returned 500") {
		t.Fatalf("err = %v", err)
	}

	f.script(status(http.StatusForbidden))
	if err := s.Notify(ctx, runEvent(RunRunning, "running")); err == nil || !strings.Contains(err.Error(), "returned 403") {
		t.Fatalf("err = %v", err)
	}
	f.script(body(`{"ok":false,"error":"channel_not_found"}`))
	if err := s.Notify(ctx, runEvent(RunRunning, "running")); err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("err = %v", err)
	}
	f.script(body(`{`))
	if err := s.Notify(ctx, runEvent(RunRunning, "running")); err == nil || !strings.Contains(err.Error(), "decode slack chat.postMessage") {
		t.Fatalf("err = %v", err)
	}
	f.script(status(500))
	s.Sleep = func(ctx context.Context, _ time.Duration) error { return errors.New("stop") }
	if err := s.Notify(ctx, runEvent(RunRunning, "running")); err == nil || !strings.HasSuffix(err.Error(), "Internal Server Error: stop") {
		t.Fatalf("err = %v", err)
	}

	s.Sleep = nil
	if err := s.sleep(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	f.script(status(500))
	if err := s.Notify(cancelled, runEvent(RunRunning, "running")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSlackDefaultBaseURL(t *testing.T) {
	t.Parallel()

	var seen string
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.URL.String()

		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"channel":"C","ts":"1"}`)), Header: http.Header{}}, nil
	})}
	s := Slack{Token: "xoxb-test", Channel: "#ops", Client: client, Store: &memTS{}}
	if err := s.Notify(context.Background(), Event{Kind: KindRun, RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	if seen != SlackAPI+"/chat.postMessage" {
		t.Fatalf("url = %q", seen)
	}
}

func TestSlackTransportErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := Slack{Token: "xoxb-test", Store: &memTS{}, BaseURL: "http://127.0.0.1:1", Sleep: noSleep}
	if err := s.Notify(ctx, Event{Kind: KindRun, RunID: "r"}); err == nil || !strings.Contains(err.Error(), "post slack chat.postMessage") {
		t.Fatalf("err = %v", err)
	}
	s.BaseURL = "://bad"
	if err := s.Notify(ctx, Event{Kind: KindRun, RunID: "r"}); err == nil || !strings.Contains(err.Error(), "build slack request") {
		t.Fatalf("err = %v", err)
	}

	f := newFakeSlack(t)
	store := &memTS{err: errors.New("db down")}
	s = newSlack(f, store, ModeThread)
	if err := s.Notify(ctx, Event{Kind: KindRun, RunID: "r"}); err == nil || err.Error() != "db down" {
		t.Fatalf("get error = %v", err)
	}
	store.err = nil
	if err := s.Notify(ctx, Event{Kind: KindRun, RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("put failed")
	if err := s.Notify(ctx, Event{Kind: KindDrift, Type: DriftDetected, Target: "app"}); err == nil || err.Error() != "put failed" {
		t.Fatalf("put error = %v", err)
	}
	store.err = nil
	f.script(status(500), status(500), status(500), status(500))
	if err := s.Notify(ctx, Event{Kind: KindRun, RunID: "r", Type: RunRunning}); err == nil || !strings.Contains(err.Error(), "returned 500") {
		t.Fatalf("reply failure must surface: %v", err)
	}
}
