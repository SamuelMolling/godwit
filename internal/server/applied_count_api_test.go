package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestAppliedCountAgreesWithTargetStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	runToSuccess(t, client, []*godwitv1.MigrationFile{
		{Name: "20260901120000_t.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "20260901120000_t.down.sql", Body: "DROP TABLE t;"},
		{Name: "R__order_statistics.up.sql", Body: "CREATE OR REPLACE VIEW order_statistics AS SELECT count(*) AS n FROM t;"},
		{Name: "R__order_statistics.down.sql", Body: "DROP VIEW IF EXISTS order_statistics;"},
		{Name: "R__order_statistical.up.sql", Body: "CREATE OR REPLACE VIEW order_statistical AS SELECT min(id) AS lo FROM t;"},
		{Name: "R__order_statistical.down.sql", Body: "DROP VIEW IF EXISTS order_statistical;"},
	}, nil)

	st, err := client.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app"}))
	if err != nil || len(st.Msg.Applied) != 3 {
		t.Fatalf("status applied = %+v, err = %v", st.Msg.GetApplied(), err)
	}
	res, err := client.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.Targets[0].AppliedCount; got != int32(len(st.Msg.Applied)) {
		t.Fatalf("targets counts %d applied, target status lists %d", got, len(st.Msg.Applied))
	}
}
