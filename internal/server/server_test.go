package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/net/http2"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

var (
	testDSN string
	testLog = slog.New(slog.NewTextHandler(io.Discard, nil))
	testKey = bytes.Repeat([]byte("k"), 32)
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("godwit"),
		tcpostgres.WithUsername("godwit"),
		tcpostgres.WithPassword("godwit"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres container required for server tests:", err)
		os.Exit(1)
	}
	testDSN, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "connection string:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = ctr.Terminate(ctx)
	os.Exit(code)
}

var dbSeq atomic.Int64

func newDatabase(t *testing.T, prefix string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	name := fmt.Sprintf("%s%d", prefix, dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}

	return strings.Replace(testDSN, "/godwit?", "/"+name+"?", 1)
}

// startService boots a replica against storeDSN and returns its base URL.
func startService(t *testing.T, storeDSN, holder string, tokens []string) string {
	t.Helper()

	return startServiceOpts(t, storeDSN, holder, tokens, testKey)
}

func startServiceWithKey(t *testing.T, storeDSN string, key []byte) string {
	t.Helper()

	return startServiceOpts(t, storeDSN, "r1", nil, key)
}

func startServiceOpts(t *testing.T, storeDSN, holder string, tokens []string, key []byte) string {
	t.Helper()

	return startServiceCfg(t, Config{
		Listen:    "127.0.0.1:0",
		StoreDSN:  storeDSN,
		MasterKey: key,
		Tokens:    tokens,
		Holder:    holder,
		Scheduler: controlplane.Config{Interval: 50 * time.Millisecond},
		Log:       testLog,
	})
}

func startServiceCfg(t *testing.T, cfg Config) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ready := make(chan net.Addr, 1)
	errCh := make(chan error, 1)
	cfg.OnReady = func(addr net.Addr) { ready <- addr }
	go func() {
		errCh <- Run(ctx, cfg)
	}()
	select {
	case addr := <-ready:
		return "http://" + addr.String()
	case err := <-errCh:
		t.Fatalf("service failed to start: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("service did not become ready")
	}

	return ""
}

func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, network, addr)
		},
	}}
}

func newClient(baseURL, token string) godwitv1connect.GodwitServiceClient {
	opts := []connect.ClientOption{}
	if token != "" {
		opts = append(opts, connect.WithInterceptors(bearerInterceptor(token)))
	}

	return godwitv1connect.NewGodwitServiceClient(h2cClient(), baseURL, opts...)
}

type bearer struct{ token string }

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)

		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)

		return conn
	}
}

func (bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func bearerInterceptor(token string) connect.Interceptor {
	return bearer{token: token}
}

func migrationFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_t.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "20260901120000_t.down.sql", Body: "DROP TABLE t;"},
	}
}

func TestServiceEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startService(t, newDatabase(t, "st"), "replica-1", []string{"tok1"})
	client := newClient(baseURL, "tok1")

	noAuth := newClient(baseURL, "")
	_, err := noAuth.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated err = %v", err)
	}

	targetDSN := newDatabase(t, "tg")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN,
	})); err != nil {
		t.Fatal(err)
	}

	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: migrationFiles(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Msg.RunId

	stream, err := client.WatchRun(ctx, connect.NewRequest(&godwitv1.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	var final godwitv1.RunState
	for stream.Receive() {
		final = stream.Msg().Run.State
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if final != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("final state = %v", final)
	}

	conn, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("table t missing: %v", err)
	}

	got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: runID}))
	if err != nil || got.Msg.Run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || got.Msg.Run.FinishedAt == nil {
		t.Fatalf("get = %+v, err = %v", got, err)
	}
	list, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "app"}))
	if err != nil || len(list.Msg.Runs) != 1 {
		t.Fatalf("list = %+v, err = %v", list, err)
	}
}

func TestServiceFailover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")

	// Replica 1: short-TTL claim then immediate death (context cancelled).
	replicaCtx, killReplica := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	go func() {
		_ = Run(replicaCtx, Config{
			Listen: "127.0.0.1:0", StoreDSN: storeDSN, MasterKey: testKey,
			Holder:    "replica-1",
			Scheduler: controlplane.Config{Interval: 50 * time.Millisecond, TTL: 2 * time.Second},
			Log:       testLog,
			OnReady:   func(addr net.Addr) { ready <- addr },
		})
	}()
	var baseURL string
	select {
	case addr := <-ready:
		baseURL = "http://" + addr.String()
	case <-time.After(15 * time.Second):
		t.Fatal("replica 1 not ready")
	}
	client := newClient(baseURL, "")

	targetDSN := newDatabase(t, "tg")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN,
	})); err != nil {
		t.Fatal(err)
	}

	// A migration slower than replica 1's remaining life.
	files := []*godwitv1.MigrationFile{
		{Name: "20260901120000_slow.up.sql", Body: "CREATE TABLE slow_t (id int); SELECT pg_sleep(3);"},
		{Name: "20260901120000_slow.down.sql", Body: "DROP TABLE slow_t;"},
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: files,
	}))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Msg.RunId

	// Wait until replica 1 has claimed it, then kill the whole replica mid-run.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: runID}))
		if err == nil && r.Msg.Run.State == godwitv1.RunState_RUN_STATE_RUNNING {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	killReplica()

	// Replica 2 must recover the abandoned lease and finish the run.
	baseURL2 := startService(t, storeDSN, "replica-2", nil)
	client2 := newClient(baseURL2, "")

	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, err := client2.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: runID}))
		if err != nil {
			t.Fatal(err)
		}
		if r.Msg.Run.State == godwitv1.RunState_RUN_STATE_SUCCEEDED {
			if r.Msg.Run.Attempts < 2 {
				t.Fatalf("attempts = %d, want >= 2 (crash + recovery)", r.Msg.Run.Attempts)
			}

			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("run never succeeded after failover")
}

func TestRunStartErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if err := Run(ctx, Config{StoreDSN: "://bad", Log: testLog}); err == nil {
		t.Fatal("bad store DSN must fail")
	}
	if err := Run(ctx, Config{StoreDSN: "postgres://bad:bad@127.0.0.1:1/x", Log: testLog}); err == nil {
		t.Fatal("unreachable store must fail")
	}
	if err := Run(ctx, Config{StoreDSN: newDatabase(t, "st"), Listen: "127.0.0.1:1", Log: testLog}); err == nil {
		t.Fatal("privileged port must fail")
	}
	if err := Run(ctx, Config{SlackMode: "shout", Log: testLog}); err == nil || !strings.Contains(err.Error(), `slack mode "shout"`) {
		t.Fatalf("bad slack mode: %v", err)
	}
	if err := Run(ctx, Config{SlackToken: "xoxb-1", Log: testLog}); err == nil || !strings.Contains(err.Error(), "slack channel is required") {
		t.Fatalf("missing channel: %v", err)
	}
}
