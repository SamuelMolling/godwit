//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

const (
	token    = "e2e-token"
	actor    = "e2e"
	pgRole   = "godwit"
	leaseTTL = 5 * time.Second
	settle   = 60 * time.Second
)

var (
	bin      string
	adminDSN string
	rigSeq   atomic.Int64
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "godwit-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	bin = filepath.Join(dir, "godwit")
	build := exec.Command("go", "build", "-o", bin, "./cmd/godwit")
	build.Dir = filepath.Join("..", "..")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build godwit:", err)

		return 1
	}

	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase(pgRole),
		tcpostgres.WithUsername(pgRole),
		tcpostgres.WithPassword(pgRole),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithCmdArgs("-c", "max_connections=300"),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres container required for e2e tests:", err)

		return 1
	}
	defer func() { _ = ctr.Terminate(ctx) }()
	if adminDSN, err = ctr.ConnectionString(ctx, "sslmode=disable"); err != nil {
		fmt.Fprintln(os.Stderr, "connection string:", err)

		return 1
	}

	return m.Run()
}

type rig struct {
	t        *testing.T
	storeDSN string
	appDSN   string
	appDB    string
	target   string
	workdir  string
	mu       sync.Mutex
	replicas []*replica
}

func newRig(t *testing.T, replicas int) *rig {
	t.Helper()
	name := fmt.Sprintf("e2e%d", rigSeq.Add(1))
	r := &rig{
		t:        t,
		storeDSN: createDatabase(t, "store_"+name),
		appDSN:   createDatabase(t, "app_"+name),
		appDB:    "app_" + name,
		workdir:  t.TempDir(),
	}
	t.Cleanup(r.stopAll)
	for range replicas {
		r.start()
	}

	return r
}

func createDatabase(t *testing.T, name string) string {
	t.Helper()
	execSQL(t, adminDSN, "CREATE DATABASE "+name)
	t.Cleanup(func() { execSQL(t, adminDSN, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })

	return strings.Replace(adminDSN, "/godwit?", "/"+name+"?", 1)
}

type replica struct {
	cmd  *exec.Cmd
	addr string
	logs *logBuffer
	dead bool
}

func (r *rig) start() {
	t := r.t
	t.Helper()
	rep := &replica{logs: &logBuffer{}}
	rep.cmd = exec.Command(bin, "serve",
		"--store-dsn", r.storeDSN,
		"--listen", "127.0.0.1:0",
		"--lease-ttl", leaseTTL.String(),
		"--tick-interval", "500ms",
		"--max-attempts", "3",
		"--drift-interval", "2s",
	)
	rep.cmd.Env = append(os.Environ(), "GODWIT_MASTER_KEY="+strings.Repeat("ab", 32), "GODWIT_TOKENS="+actor+":"+token)
	rep.cmd.Stderr = rep.logs
	if err := rep.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rep.addr = await(t, 15*time.Second, "replica listening", func() (string, bool) {
		addr, ok := rep.logs.field("listening", "addr")

		return addr, ok
	})
	waitUntil(t, 15*time.Second, "replica ready", func() bool {
		resp, err := http.Get("http://" + rep.addr + "/readyz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	})
	r.mu.Lock()
	r.replicas = append(r.replicas, rep)
	r.mu.Unlock()
}

func (r *rig) kill(rep *replica) {
	r.t.Helper()
	if err := rep.cmd.Process.Kill(); err != nil {
		r.t.Fatal(err)
	}
	_ = rep.cmd.Wait()
	r.mu.Lock()
	rep.dead = true
	r.mu.Unlock()
}

func (r *rig) stopAll() {
	r.mu.Lock()
	reps := append([]*replica(nil), r.replicas...)
	r.mu.Unlock()
	for _, rep := range reps {
		if !rep.dead {
			r.kill(rep)
		}
	}
}

func (r *rig) alive() *replica {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rep := range r.replicas {
		if !rep.dead {
			return rep
		}
	}
	r.t.Fatal("no replica alive")

	return nil
}

func (r *rig) claimer(runID string) *replica {
	r.t.Helper()

	return await(r.t, settle, "run "+runID+" claimed", func() (*replica, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, rep := range r.replicas {
			if rep.logs.has("run claimed", "run", runID) && !rep.dead {
				return rep, true
			}
		}

		return nil, false
	})
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *logBuffer) field(msg, key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, entry := range b.entries(msg) {
		if v, ok := entry[key].(string); ok {
			return v, true
		}
	}

	return "", false
}

func (b *logBuffer) has(msg, key, value string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, entry := range b.entries(msg) {
		if entry[key] == value {
			return true
		}
	}

	return false
}

func (b *logBuffer) entries(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(b.buf.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == msg {
			out = append(out, entry)
		}
	}

	return out
}

func (r *rig) cli(args ...string) (int, string, string) {
	t := r.t
	t.Helper()
	cmd := exec.Command(bin, append(args, "--server", "http://"+r.alive().addr, "--token", token)...)
	cmd.Dir = r.workdir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	code := 0
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	t.Logf("$ godwit %s (exit %d)\n%s%s", strings.Join(args, " "), code, out.String(), errOut.String())

	return code, out.String(), errOut.String()
}

func (r *rig) mustCLI(args ...string) string {
	r.t.Helper()
	code, out, errOut := r.cli(args...)
	if code != 0 {
		r.t.Fatalf("godwit %s: exit %d: %s", strings.Join(args, " "), code, errOut)
	}

	return out
}

func (r *rig) addTarget(name string) {
	r.t.Helper()
	r.target = name
	r.mustCLI("target", "add", name, "--provider", "static", "--dsn", r.appDSN)
}

func (r *rig) migrate(dir string, extra ...string) (int, string, string) {
	r.t.Helper()

	return r.cli(append([]string{"migrate", "--target", r.target, "--dir", dir}, extra...)...)
}

func (r *rig) mustMigrate(dir string, extra ...string) string {
	r.t.Helper()

	return r.mustCLI(append([]string{"migrate", "--target", r.target, "--dir", dir}, extra...)...)
}

type bearer struct{ next http.RoundTripper }

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+token)

	return b.next.RoundTrip(req)
}

func (r *rig) client() godwitv1connect.GodwitServiceClient {
	r.t.Helper()

	return godwitv1connect.NewGodwitServiceClient(
		&http.Client{Transport: bearer{next: http.DefaultTransport}}, "http://"+r.alive().addr)
}

func (r *rig) metrics() string {
	r.t.Helper()
	resp, err := http.Get("http://" + r.alive().addr + "/metrics")
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}

	return string(body)
}

func (r *rig) createRun(migs ...migration) string {
	r.t.Helper()
	resp, err := r.client().CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: r.target, Files: files(migs...),
	}))
	if err != nil {
		r.t.Fatal(err)
	}

	return resp.Msg.RunId
}

func (r *rig) getRun(id string) *godwitv1.Run {
	r.t.Helper()
	resp, err := r.client().GetRun(context.Background(), connect.NewRequest(&godwitv1.GetRunRequest{RunId: id}))
	if err != nil {
		r.t.Fatal(err)
	}

	return resp.Msg.Run
}

func (r *rig) listRuns() []*godwitv1.Run {
	r.t.Helper()
	resp, err := r.client().ListRuns(context.Background(), connect.NewRequest(&godwitv1.ListRunsRequest{Target: r.target}))
	if err != nil {
		r.t.Fatal(err)
	}

	return resp.Msg.Runs
}

func (r *rig) latestRun() *godwitv1.Run {
	r.t.Helper()
	runs := r.listRuns()
	if len(runs) == 0 {
		r.t.Fatalf("no runs on target %s", r.target)
	}

	return runs[0]
}

func (r *rig) driftEvents() []*godwitv1.DriftEvent {
	r.t.Helper()
	resp, err := r.client().ListDriftEvents(context.Background(), connect.NewRequest(&godwitv1.ListDriftEventsRequest{Target: r.target}))
	if err != nil {
		r.t.Fatal(err)
	}

	return resp.Msg.Events
}

func (r *rig) waitActive(queryPrefix string) {
	r.t.Helper()
	waitUntil(r.t, settle, "statement "+queryPrefix+" active", func() bool {
		return query[int](r.t, adminDSN,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND state = 'active' AND query LIKE $2`,
			r.appDB, queryPrefix+"%") > 0
	})
}

func (r *rig) expectRun(id string, state godwitv1.RunState, attempts int32) {
	r.t.Helper()
	run := r.getRun(id)
	if run.State != state || run.Attempts != attempts {
		r.t.Fatalf("run %s: state %s attempts %d, want %s/%d (error: %s)", id, run.State, run.Attempts, state, attempts, run.Error)
	}
}

type migration struct {
	version int64
	name    string
	up      string
	down    string
}

func files(migs ...migration) []*godwitv1.MigrationFile {
	var out []*godwitv1.MigrationFile
	for _, m := range migs {
		out = append(out,
			&godwitv1.MigrationFile{Name: fmt.Sprintf("%014d_%s.up.sql", m.version, m.name), Body: m.up},
			&godwitv1.MigrationFile{Name: fmt.Sprintf("%014d_%s.down.sql", m.version, m.name), Body: m.down},
		)
	}

	return out
}

func migrationDir(t *testing.T, migs ...migration) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files(migs...) {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func connectDB(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	return conn
}

func execSQL(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func query[T any](t *testing.T, dsn, sql string, args ...any) T {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var v T
	if err := conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}

	return v
}

func await[T any](t *testing.T, timeout time.Duration, what string, fn func() (T, bool)) T {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v, ok := fn()
		if ok {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	await(t, timeout, what, func() (struct{}, bool) { return struct{}{}, fn() })
}

func expectContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}
