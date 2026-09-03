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
	out controlplane.SchemaDiff
	err error
}

func (d stubDiffer) Diff(context.Context, string, string) (controlplane.SchemaDiff, error) {
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
		{"bad ddl", stubDiffer{err: fmt.Errorf("%w: type nosuchtype", controlplane.ErrDesiredSchema)}, diffReq("app", "x"), connect.CodeInvalidArgument},
		{"unknown target", stubDiffer{err: controlplane.ErrNotFound}, diffReq("ghost", "x"), connect.CodeNotFound},
		{"target down", stubDiffer{err: errors.New("connect target")}, diffReq("app", "x"), connect.CodeInternal},
		{"unnamed index", stubDiffer{out: controlplane.SchemaDiff{UpSQL: "CREATE INDEX CONCURRENTLY ON t (a);"}}, diffReq("app", "x"), connect.CodeInternal},
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
	s.Differ = stubDiffer{out: controlplane.SchemaDiff{
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

	s.Differ = stubDiffer{out: controlplane.SchemaDiff{Observed: controlplane.Observation{At: at}}}
	resp, err = s.Diff(ctx, diffReq("app", "CREATE TABLE t (b int);"))
	if err != nil || resp.Msg.UpSql != "" || resp.Msg.DownSql != "" || len(resp.Msg.Statements) != 0 || resp.Msg.Drift != "" {
		t.Fatalf("no changes: resp = %+v, err = %v", resp.Msg, err)
	}
}
