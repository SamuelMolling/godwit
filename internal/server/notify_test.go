package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/notify"
)

type recordedCall struct {
	Method  string
	Payload map[string]any
}

type fakeReceiver struct {
	t     *testing.T
	token string
	mu    sync.Mutex
	calls []recordedCall
	seq   int
}

func (f *fakeReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		f.t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
	}
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.calls = append(f.calls, recordedCall{Method: strings.TrimPrefix(r.URL.Path, "/"), Payload: payload})
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C1", "ts": fmt.Sprintf("1.%d", f.seq)})
}

func (f *fakeReceiver) waitFor(t *testing.T, text string) []recordedCall {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		calls := append([]recordedCall(nil), f.calls...)
		f.mu.Unlock()
		for _, c := range calls {
			if c.Payload["text"] == text {
				return calls
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never received %q", text)

	return nil
}

type eventRecorder struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *eventRecorder) Notify(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)

	return nil
}

func (r *eventRecorder) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Kind+":"+e.Type)
	}

	return strings.Join(out, " ")
}

func TestNotificationsEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	slack := &fakeReceiver{t: t, token: "xoxb-test"}
	slackSrv := httptest.NewServer(slack)
	t.Cleanup(slackSrv.Close)
	hook := &fakeReceiver{t: t}
	hookSrv := httptest.NewServer(hook)
	t.Cleanup(hookSrv.Close)
	rec := &eventRecorder{}

	storeDSN := newDatabase(t, "st")
	baseURL := startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: storeDSN, MasterKey: testKey, Holder: "r1",
		Scheduler:  controlplane.Config{Interval: 50 * time.Millisecond},
		WebhookURL: hookSrv.URL, SlackToken: "xoxb-test", SlackChannel: "#ops", SlackURL: slackSrv.URL,
		PublicURL: "https://godwit.example.com", Notifier: rec, Log: testLog,
	})
	client := newClient(baseURL, "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: migrationFiles()}))
	if err != nil {
		t.Fatal(err)
	}
	firstID := created.Msg.RunId
	watchToEnd(t, client, firstID)

	files := []*godwitv1.MigrationFile{
		{Name: "20260901130000_add.up.sql", Body: "ALTER TABLE t ADD COLUMN new_id int;"},
		{Name: "20260901130000_add.down.sql", Body: "ALTER TABLE t DROP COLUMN new_id;"},
		{Name: "20260901130001_drop.up.sql", Body: "ALTER TABLE t DROP COLUMN id;"},
		{Name: "20260901130001_drop.down.sql", Body: "ALTER TABLE t ADD COLUMN id int;"},
	}
	created, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files, Rollout: controlplane.RolloutExpandContract, AcknowledgeHazards: []string{"H003"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	watchToEnd(t, client, created.Msg.RunId)
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: created.Msg.RunId})); err != nil {
		t.Fatal(err)
	}
	watchToEnd(t, client, created.Msg.RunId)

	created, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: []*godwitv1.MigrationFile{
			{Name: "20260901140000_bad.up.sql", Body: "SELECT 1/0;"},
			{Name: "20260901140000_bad.down.sql", Body: "SELECT 1;"},
		},
		SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	badID := created.Msg.RunId
	watchToEnd(t, client, badID)
	if _, err := client.ResumeRun(ctx, connect.NewRequest(&godwitv1.ResumeRunRequest{RunId: badID})); err != nil {
		t.Fatal(err)
	}
	watchToEnd(t, client, badID)
	if _, err := client.ParkRun(ctx, connect.NewRequest(&godwitv1.ParkRunRequest{RunId: badID, Reason: "manual hold"})); err != nil {
		t.Fatal(err)
	}
	reverted, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: badID}))
	if err != nil {
		t.Fatal(err)
	}
	watchToEnd(t, client, reverted.Msg.RunId)

	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE TABLE rogue (id int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"})); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "DROP TABLE rogue"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: "app"})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "legacy", Provider: "static", Dsn: newDatabase(t, "lg"),
	})); err != nil {
		t.Fatal(err)
	}
	baselined, err := client.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "legacy", Files: baselineFiles(), Version: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}

	want := "run:created run:running run:succeeded " +
		"run:created run:running run:awaiting_contract run:confirmed run:running run:succeeded " +
		"run:created run:running run:failed run:resumed run:running run:failed run:parked " +
		"run:created run:running run:succeeded run:reverted " +
		"drift:detected drift:resolved drift:accepted " +
		"run:succeeded"
	if got := rec.summary(); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
	rec.mu.Lock()
	parked, revert, orig, base := rec.events[15], rec.events[16], rec.events[19], rec.events[23]
	rec.mu.Unlock()
	if base.RunID != baselined.Msg.RunId || base.Target != "legacy" || base.Detail != "baseline to version 1: 1 migrations marked applied" {
		t.Fatalf("baseline = %+v", base)
	}
	if parked.RunID != badID || parked.Detail != "manual hold" || parked.State != controlplane.StateNeedsAttention {
		t.Fatalf("parked = %+v", parked)
	}
	if revert.RunID != reverted.Msg.RunId || revert.Detail != "reverts run "+notify.ShortID(badID) {
		t.Fatalf("revert = %+v", revert)
	}
	if orig.RunID != badID || orig.State != controlplane.StateReverted {
		t.Fatalf("reverted = %+v", orig)
	}

	calls := slack.waitFor(t, "godwit drift accepted on app")
	var rootTS string
	var replies, updates int
	for _, c := range calls {
		p := c.Payload
		if p["text"] == "godwit run created on app (run "+notify.ShortID(firstID)+")" {
			if c.Method != "chat.postMessage" || p["channel"] != "#ops" {
				t.Fatalf("root = %+v", c)
			}
			rootTS = fmt.Sprintf("1.%d", 1)
		}
		if !strings.Contains(p["text"].(string), notify.ShortID(firstID)) {
			continue
		}
		switch {
		case c.Method == "chat.postMessage" && p["thread_ts"] == rootTS:
			replies++
		case c.Method == "chat.update" && p["ts"] == rootTS:
			updates++
		}
	}
	if rootTS == "" || replies != 2 || updates != 2 {
		t.Fatalf("first run: root %q, replies %d, updates %d", rootTS, replies, updates)
	}
	button := calls[0].Payload["blocks"].([]any)[2].(map[string]any)["elements"].([]any)[0].(map[string]any)
	if button["url"] != "https://godwit.example.com/ui/runs/"+firstID {
		t.Fatalf("button = %v", button)
	}

	hookCalls := hook.waitFor(t, "godwit drift accepted on app")
	if hookCalls[0].Payload["kind"] != "run" || hookCalls[0].Payload["type"] != "created" || hookCalls[0].Payload["run_id"] != firstID {
		t.Fatalf("webhook first = %+v", hookCalls[0])
	}

	var channel, ts string
	storeConn, err := pgx.Connect(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeConn.Close(ctx) }()
	if err := storeConn.QueryRow(ctx, "SELECT channel, ts FROM cp_notifications WHERE kind = 'run' AND key = $1", firstID).Scan(&channel, &ts); err != nil {
		t.Fatal(err)
	}
	if channel != "C1" || ts != rootTS {
		t.Fatalf("stored = %s %s", channel, ts)
	}

	body := scrapeMetrics(t, baseURL)
	for _, want := range []string{
		`godwit_notifications_total{provider="slack",result="delivered"}`,
		`godwit_notifications_total{provider="webhook",result="delivered"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}
