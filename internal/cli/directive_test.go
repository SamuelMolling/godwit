package cli

import (
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestRunLineShowsBackfillProgress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		progress *godwitv1.RunProgress
		want     string
	}{
		{nil, "run r1: running"},
		{&godwitv1.RunProgress{Migration: "m", Statement: 2}, "[statement 2 of m]"},
		{&godwitv1.RunProgress{Batches: 3, RowsDone: 30}, "[backfill 30 rows (batch 3)]"},
		{&godwitv1.RunProgress{Batches: 64, RowsDone: 320000, RowsTotal: 1240000}, "[backfill 320000/~1240000 rows (batch 64)]"},
	} {
		got := runLine(&godwitv1.Run{Id: "r1", State: godwitv1.RunState_RUN_STATE_RUNNING, Progress: tc.progress})
		if !strings.Contains(got, tc.want) {
			t.Fatalf("line = %q, want %q", got, tc.want)
		}
	}
}

func TestTargetAddKeepOldFlag(t *testing.T) {
	t.Parallel()
	stub := &stubService{}
	url := startStub(t, stub)

	if code, _, errOut := runCLI("target", "add", "app", "--server", url, "--provider", "static", "--dsn", "x", "--keep-old=false"); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errOut)
	}
	if got := stub.registered.KeepOld; got == nil || *got {
		t.Fatalf("keep_old = %v", got)
	}
	if code, _, errOut := runCLI("target", "add", "app", "--server", url, "--provider", "static", "--dsn", "x"); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errOut)
	}
	if got := stub.registered.KeepOld; got != nil {
		t.Fatalf("an untouched flag must leave the target's default alone: %v", *got)
	}
	if got := stub.registered.IgnoreAdoptedTables; got != nil {
		t.Fatalf("an untouched flag must leave the target's default alone: %v", *got)
	}

	if code, _, errOut := runCLI("target", "add", "app", "--server", url, "--provider", "static", "--dsn", "x",
		"--ignore-adopted-tables=false"); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errOut)
	}
	if got := stub.registered.IgnoreAdoptedTables; got == nil || *got {
		t.Fatalf("ignore_adopted_tables = %v", got)
	}
}
