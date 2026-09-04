package server

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/api"
)

// Nothing bounded a request before: the body was buffered whole, then multiplied through the proto
// message, the file map, the loader's in-memory FS and the split statements.
func TestAPIRefusesOversizedRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startServiceCfg(t, Config{
		Listen: "127.0.0.1:0", StoreDSN: newDatabase(t, "st"), Keys: testKeys, Holder: "r1", Log: testLog,
		Limits: api.Limits{RequestBytes: 4096, Files: 4, FileBytes: 512},
	})
	client := newClient(baseURL, "")
	registerTarget(t, client, newDatabase(t, "tg"))

	big := []*godwitv1.MigrationFile{
		{Name: "20260901120000_t.up.sql", Body: "-- " + strings.Repeat("x", 600) + "\nCREATE TABLE t (id int);"},
		{Name: "20260901120000_t.down.sql", Body: "DROP TABLE t;"},
	}
	_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: big}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "bytes, limit 512") {
		t.Fatalf("oversized file: %v", err)
	}

	many := make([]*godwitv1.MigrationFile, 0, 6)
	for i := range 3 {
		v := "2026090112000" + strconv.Itoa(i)
		many = append(many,
			&godwitv1.MigrationFile{Name: v + "_t.up.sql", Body: "CREATE TABLE t" + strconv.Itoa(i) + " (id int);"},
			&godwitv1.MigrationFile{Name: v + "_t.down.sql", Body: "DROP TABLE t" + strconv.Itoa(i) + ";"})
	}
	_, err = client.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: many}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "too many migration files") {
		t.Fatalf("too many files: %v", err)
	}

	_, err = client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{
		Target: "app", Schema: "-- " + strings.Repeat("y", 600),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "schema is") {
		t.Fatalf("oversized schema: %v", err)
	}

	_, err = client.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{Target: "app", Schema: "CREATE TABLE t (id int);", Files: many}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "too many migration files") {
		t.Fatalf("too many diff files: %v", err)
	}

	_, err = client.Checkpoint(ctx, connect.NewRequest(&godwitv1.CheckpointRequest{Name: "squash", Files: many}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "too many migration files") {
		t.Fatalf("too many checkpoint files: %v", err)
	}

	// The transport refuses what never reaches a handler.
	_, err = client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: []*godwitv1.MigrationFile{{Name: "20260901120000_t.up.sql", Body: strings.Repeat("z", 8192)}},
	}))
	if err == nil {
		t.Fatal("a body over --max-request-bytes must be refused by the transport")
	}
}

func TestOpenPoolRejectsAnEmptyPool(t *testing.T) {
	t.Parallel()

	if _, err := openPool(context.Background(), newDatabase(t, "st"), 0); err == nil {
		t.Fatal("a pool that can hold no connection must be refused")
	}
}
