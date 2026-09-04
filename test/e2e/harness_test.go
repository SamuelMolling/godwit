//go:build load || chaos

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

// converge bounds every wait in the load and chaos rigs; a run that has not settled by then is a finding.
const converge = 10 * time.Minute

const bfTable = "CREATE TABLE bf (id bigserial PRIMARY KEY, v int NOT NULL, w bigint);"

func backfillMigration(version int64, batch int, extra string) migration {
	return migration{
		version, "backfill_w",
		fmt.Sprintf("-- godwit: backfill bf set='w = v * 2' where='w IS DISTINCT FROM v * 2' key=id batch=%d%s\n", batch, extra),
		"UPDATE bf SET w = NULL;",
	}
}

func watchRun(t *testing.T, r *rig, id string) (*godwitv1.Run, time.Duration) {
	t.Helper()
	start := time.Now()
	var run *godwitv1.Run
	waitUntil(t, converge, "run "+id+" settled", func() bool {
		run = r.getRun(id)
		switch run.State {
		case godwitv1.RunState_RUN_STATE_SUCCEEDED, godwitv1.RunState_RUN_STATE_FAILED,
			godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION, godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT,
			godwitv1.RunState_RUN_STATE_REVERTED:
			return true
		default:
			return false
		}
	})

	return run, time.Since(start)
}

func (r *rig) useReplica(rep *replica) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefer = rep
}

func (r *rig) all() []*replica {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]*replica(nil), r.replicas...)
}

func (b *logBuffer) count(msg string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.entries(msg))
}

func (b *logBuffer) countField(msg, key, value string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, entry := range b.entries(msg) {
		if entry[key] == value {
			n++
		}
	}

	return n
}

// report prints one measurement line. `make load` and `make chaos` grep for the prefix.
func report(t *testing.T, name string, kv ...any) {
	t.Helper()
	var b strings.Builder
	b.WriteString("RIG ")
	b.WriteString(name)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %v=%v", kv[i], kv[i+1])
	}
	t.Log(b.String())
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		panic(name + ": " + err.Error())
	}

	return n
}

func timed(fn func()) time.Duration {
	start := time.Now()
	fn()

	return time.Since(start)
}

func holdLock(t *testing.T, dsn, sql string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			_ = conn.Close(ctx)
		})
	}
	t.Cleanup(release)

	return release
}

func seedRows(t *testing.T, dsn, table string, n int) time.Duration {
	t.Helper()

	return timed(func() {
		execSQL(t, dsn, fmt.Sprintf(
			`INSERT INTO %s (v) SELECT (g %% 1000)::int FROM generate_series(1, %d) g`, table, n))
	})
}

// probe is query without a testing.T: safe to call from a sampling goroutine, zero on any failure.
func probe[T any](dsn, sql string, args ...any) T {
	var v T
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return v
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		var zero T

		return zero
	}

	return v
}

func rss(pid int) int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}

	return kb
}

func relSize(t *testing.T, dsn, rel string) int64 {
	t.Helper()

	return query[int64](t, dsn, `SELECT pg_total_relation_size($1)`, rel)
}

func backends(t *testing.T, db string) int {
	t.Helper()

	return query[int](t, adminDSN, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1`, db)
}

func terminate(t *testing.T, db, like string) int {
	t.Helper()

	return query[int](t, adminDSN, `
		SELECT count(*) FROM (
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND pid <> pg_backend_pid() AND query LIKE $2) k`, db, like)
}

const (
	proxyPass = iota
	proxyHang
	proxyCut
)

// faultProxy makes a database unreachable the way a network does rather than the way a kill does.
type faultProxy struct {
	ln       net.Listener
	upstream string
	mode     atomic.Int32
	mu       sync.Mutex
	conns    []net.Conn
	closed   atomic.Bool
}

func newProxy(t *testing.T, dsn string) (*faultProxy, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{ln: ln, upstream: u.Host}
	t.Cleanup(p.close)
	go p.serve()
	u.Host = ln.Addr().String()

	return p, u.String()
}

func (p *faultProxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.mode.Load() == proxyCut {
			_ = conn.Close()

			continue
		}
		go p.handle(conn)
	}
}

func (p *faultProxy) handle(down net.Conn) {
	up, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = down.Close()

		return
	}
	p.track(down, up)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pipe(up, down) }()
	go func() { defer wg.Done(); p.pipe(down, up) }()
	wg.Wait()
	_, _ = down.Close(), up.Close()
}

func (p *faultProxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = append(p.conns, conns...)
}

func (p *faultProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := src.Read(buf)
		for p.mode.Load() == proxyHang && !p.closed.Load() {
			time.Sleep(20 * time.Millisecond)
		}
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err == nil {
			continue
		}
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() || p.mode.Load() == proxyCut || p.closed.Load() {
			return
		}
	}
}

// set changes what the proxy does to traffic; proxyCut also severs the sessions already open.
func (p *faultProxy) set(mode int32) {
	p.mode.Store(mode)
	if mode != proxyCut {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *faultProxy) close() {
	p.closed.Store(true)
	_ = p.ln.Close()
	p.set(proxyCut)
	p.mode.Store(proxyPass)
}
