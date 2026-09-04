package cli

import (
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func withheldStub() *stubService {
	return &stubService{plan: &godwitv1.PlanRunResponse{
		Target: "app", Rollout: "direct", Validated: true, PlanId: "p1", PlanKey: "k1",
		Migrations: []*godwitv1.PlannedMigration{
			{Version: 20260901120000, Name: "users", Checksum: "c1", Phase: "expand", Statements: []*godwitv1.PlannedStatement{
				{Sql: "CREATE TABLE users (id int)"},
			}},
			{Version: 20260901120001, Name: "drop_a", Checksum: "c2", Withheld: true},
			{Name: "v", Repeatable: true, Checksum: "c3", Withheld: true},
		},
	}}
}

func TestPlanWithAVersionTargetNamesWhatItWithheld(t *testing.T) {
	t.Parallel()
	stub := withheldStub()
	url := startStub(t, stub)

	code, out, errOut := runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t), "--to", "20260901120000")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.planned.ToVersion != 20260901120000 {
		t.Fatalf("to_version = %d", stub.planned.ToVersion)
	}
	want := "plan p1 on app (rollout direct, validated on a scratch database)\n" +
		"key: k1\n" +
		"withheld: 2 migration(s) in the directory this plan does not cover (20260901120001_drop_a, R__v)\n" +
		"20260901120000_users (up): 1 statement(s) [expand, pending]\n" +
		"  [0] tx    CREATE TABLE users (id int)\n" +
		"20260901120001_drop_a (up): 0 statement(s) [withheld]\n" +
		"R__v (up): 0 statement(s) [withheld]\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestPlanWithAVersionTargetInMarkdownAndJSON(t *testing.T) {
	t.Parallel()
	url := startStub(t, withheldStub())

	_, out, _ := runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t), "--to", "20260901120000", "--format", "markdown")
	if !strings.Contains(out, "**withheld: 2 migration(s) in the directory this plan does not cover (20260901120001_drop_a, R__v)**") {
		t.Fatalf("out = %q", out)
	}

	_, out, _ = runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t), "--to", "20260901120000", "--format", "json")
	if !strings.Contains(out, `"withheld":true`) {
		t.Fatalf("out = %q", out)
	}
}

func TestMigrateSendsTheVersionTarget(t *testing.T) {
	t.Parallel()
	stub := &stubService{events: []*godwitv1.Run{run("r1", godwitv1.RunState_RUN_STATE_SUCCEEDED, 0)}}
	url := startStub(t, stub)

	code, _, errOut := runCLI("migrate", "--server", url, "--target", "app", "--dir", goodMigs(t), "--to", "20260901120000")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.created.ToVersion != 20260901120000 {
		t.Fatalf("to_version = %d", stub.created.ToVersion)
	}
}

func TestVersionTargetRefusals(t *testing.T) {
	t.Parallel()
	dir := goodMigs(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"not a version", []string{"migrate", "--target", "app", "--dir", dir, "--to", "0"}, "the 14 digits its file name starts with"},
		{"on a stored plan", []string{"migrate", "--target", "app", "--plan", "p1", "--to", "1"}, "the stored plan already fixes the set it covers"},
		{"without a target", []string{"plan", "--dir", dir, "--to", "1"}, "--to needs --target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, _, errOut := runCLI(append(tc.args, "--server", "http://127.0.0.1:1")...)
			if code != 1 || !strings.Contains(errOut, tc.want) {
				t.Fatalf("code = %d, stderr = %s", code, errOut)
			}
		})
	}
}
