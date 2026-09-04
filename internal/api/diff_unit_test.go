package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type stubDiffer struct {
	out   controlplane.SchemaDiff
	err   error
	base  controlplane.DiffBase
	files map[string]string
}

func (d *stubDiffer) Diff(_ context.Context, _, _ string, base controlplane.DiffBase, files map[string]string) (controlplane.SchemaDiff, error) {
	d.base, d.files = base, files

	return d.out, d.err
}

func diffReq(target, schema string) *connect.Request[godwitv1.DiffRequest] {
	return connect.NewRequest(&godwitv1.DiffRequest{Target: target, Schema: schema})
}

func TestDiffErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := planServer(t, controlplane.Observation{}, nil)

	for _, tc := range []struct {
		name   string
		differ Differ
		req    *connect.Request[godwitv1.DiffRequest]
		code   connect.Code
	}{
		{"no target", nil, diffReq("", "CREATE TABLE t (id int);"), connect.CodeInvalidArgument},
		{"blank schema", nil, diffReq("app", " \n"), connect.CodeInvalidArgument},
		{"disabled", nil, diffReq("app", "CREATE TABLE t (id int);"), connect.CodeUnimplemented},
		{"bad ddl", &stubDiffer{err: fmt.Errorf("%w: type nosuchtype", controlplane.ErrDesiredSchema)}, diffReq("app", "x"), connect.CodeInvalidArgument},
		{"unknown target", &stubDiffer{err: controlplane.ErrNotFound}, diffReq("ghost", "x"), connect.CodeNotFound},
		{"target down", &stubDiffer{err: errors.New("connect target")}, diffReq("app", "x"), connect.CodeInternal},
		{"unnamed index", &stubDiffer{out: controlplane.SchemaDiff{UpSQL: "CREATE INDEX CONCURRENTLY ON t (a);"}}, diffReq("app", "x"), connect.CodeInternal},
		{"files base with no files", &stubDiffer{}, connect.NewRequest(&godwitv1.DiffRequest{
			Target: "app", Schema: "x", Base: godwitv1.DiffBase_DIFF_BASE_FILES,
		}), connect.CodeInvalidArgument},
		{"files do not replay", &stubDiffer{err: fmt.Errorf("%w: boom", controlplane.ErrMigrationFiles)}, diffReq("app", "x"), connect.CodeInvalidArgument},
		{"validation disabled", &stubDiffer{err: controlplane.ErrValidationDisabled}, diffReq("app", "x"), connect.CodeFailedPrecondition},
		{"repeatable does not build", &stubDiffer{err: fmt.Errorf("%w: boom", controlplane.ErrRepeatableSchema)}, diffReq("app", "x"), connect.CodeInvalidArgument},
		{"no migration directory", &stubDiffer{err: fmt.Errorf("%w: R__v", controlplane.ErrRepeatablesUnknown)}, diffReq("app", "x"), connect.CodeFailedPrecondition},
	} {
		s.Differ = tc.differ
		if _, err := s.Diff(ctx, tc.req); connect.CodeOf(err) != tc.code {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
}

func TestDiffReportsStatementsAndDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := planServer(t, controlplane.Observation{}, nil)
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	s.Differ = &stubDiffer{out: controlplane.SchemaDiff{
		Observed: controlplane.Observation{Applied: []engine.Applied{{Version: 20260901120000, Name: "t", Checksum: "c"}}, Fingerprint: "fp", At: at},
		UpSQL:    "ALTER TABLE t DROP COLUMN a;\nCREATE INDEX CONCURRENTLY t_b_idx ON t (b);",
		DownSQL:  "ALTER TABLE t ADD COLUMN a int;",
		Drift:    []string{"+ column public.t.extra integer null=YES default=<none>"},
	}}

	resp, err := s.Diff(ctx, diffReq("app", "CREATE TABLE t (b int);"))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if m.Target != "app" || m.DownSql != "ALTER TABLE t ADD COLUMN a int;" || m.Drift != "+ column public.t.extra integer null=YES default=<none>" {
		t.Fatalf("resp = %+v", m)
	}
	if len(m.Statements) != 2 || m.Statements[0].Hazards[0].Code != "H003" || m.Statements[0].Hazards[0].Recipe == "" ||
		!m.Statements[1].NoTx || len(m.Statements[1].Hazards) != 0 {
		t.Fatalf("statements = %+v", m.Statements)
	}
	if m.Observed.NewestApplied != 20260901120000 || m.Observed.AppliedCount != 1 || m.Observed.SchemaFingerprint != "fp" || !m.Observed.At.AsTime().Equal(at) {
		t.Fatalf("observed = %+v", m.Observed)
	}

	s.Differ = &stubDiffer{out: controlplane.SchemaDiff{Observed: controlplane.Observation{At: at}}}
	resp, err = s.Diff(ctx, diffReq("app", "CREATE TABLE t (b int);"))
	if err != nil || resp.Msg.UpSql != "" || resp.Msg.DownSql != "" || len(resp.Msg.Statements) != 0 || resp.Msg.Drift != "" {
		t.Fatalf("no changes: resp = %+v, err = %v", resp.Msg, err)
	}
}

func TestDiffPassesTheCommittedFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := planServer(t, controlplane.Observation{}, nil)
	stub := &stubDiffer{out: controlplane.SchemaDiff{Observed: controlplane.Observation{}}}
	s.Differ = stub

	if _, err := s.Diff(ctx, diffReq("app", "CREATE TABLE t (b int);")); err != nil {
		t.Fatal(err)
	}
	if stub.base != controlplane.DiffBaseLive || len(stub.files) != 0 {
		t.Fatalf("default base = %v, files = %v", stub.base, stub.files)
	}

	_, err := s.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{
		Target: "app", Schema: "CREATE TABLE t (b int);", Base: godwitv1.DiffBase_DIFF_BASE_FILES,
		Files: []*godwitv1.MigrationFile{{Name: "20260901120000_t.up.sql", Body: "CREATE TABLE t (b int);"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if stub.base != controlplane.DiffBaseFiles || stub.files["20260901120000_t.up.sql"] != "CREATE TABLE t (b int);" {
		t.Fatalf("files base = %v, files = %v", stub.base, stub.files)
	}

	stub.out = controlplane.SchemaDiff{RepeatableObjects: []string{"public.t_totals"}}
	resp, err := s.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{
		Target: "app", Schema: "CREATE TABLE t (b int);",
		Files: []*godwitv1.MigrationFile{{Name: "R__t_totals.up.sql", Body: totalsUpSQL}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if stub.base != controlplane.DiffBaseLive || stub.files["R__t_totals.up.sql"] != totalsUpSQL {
		t.Fatalf("live base with files = %v, files = %v", stub.base, stub.files)
	}
	if len(resp.Msg.RepeatableObjects) != 1 || resp.Msg.RepeatableObjects[0] != "public.t_totals" {
		t.Fatalf("repeatable objects = %v", resp.Msg.RepeatableObjects)
	}
}

const totalsUpSQL = "CREATE OR REPLACE VIEW t_totals AS SELECT b FROM t;"
