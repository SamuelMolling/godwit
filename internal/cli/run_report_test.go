package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var reportStart = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func reportRun(state godwitv1.RunState) *godwitv1.Run {
	return &godwitv1.Run{
		Id: "r1", Target: "app", State: state, Kind: "migrate", PlanId: "p1", Rollout: "direct",
		Source:    "github.com/acme/app@0419cdd1c2f3a4b5c6d7e8f90123456789abcdef:db/migrations",
		CreatedAt: timestamppb.New(reportStart), FinishedAt: timestamppb.New(reportStart.Add(2 * time.Second)),
	}
}

func reportItem(version int64, name string, statements ...engine.Statement) planItem {
	return planItem{Plan: engine.Plan{
		Migration:  engine.Migration{Version: version, Name: name},
		Direction:  engine.DirectionUp,
		Statements: statements,
	}}
}

func ledgerRow(id string) *godwitv1.RunMigration {
	return &godwitv1.RunMigration{Migration: id, AppliedAt: timestamppb.New(reportStart)}
}

func succeededReport() runReport {
	users := reportItem(20260901120000, "users", engine.Statement{SQL: "CREATE TABLE users (id bigint)"})
	users.changes = []engine.ObjectChange{{Op: engine.OpCreate, Kind: engine.KindTable, Schema: "public", Name: "users"}}

	return runReport{
		command: "apply",
		run:     reportRun(godwitv1.RunState_RUN_STATE_SUCCEEDED),
		applied: []*godwitv1.RunMigration{ledgerRow("20260901120000_users")},
		plan:    planReport{live: true, target: "app", rollout: "direct", validated: true, items: []planItem{users}},
		public:  "https://godwit.example.com/",
	}
}

func heldReport() runReport {
	people := reportItem(20260901120000, "people",
		engine.Statement{SQL: "ALTER TABLE people ADD COLUMN age_new bigint", Phase: engine.PhaseExpand},
		engine.Statement{SQL: "ALTER TABLE people RENAME COLUMN age TO age_old", Phase: engine.PhaseContract},
	)
	people.expanded = true
	row := ledgerRow("20260901120000_people")
	row.Held = true

	return runReport{
		command: "apply",
		run:     reportRun(godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT),
		applied: []*godwitv1.RunMigration{row},
		plan: planReport{
			live: true, target: "app", rollout: "expand-contract", validated: true, items: []planItem{people},
		},
	}
}

func failedReport() runReport {
	first := reportItem(20260901120000, "note", engine.Statement{SQL: "ALTER TABLE widgets ADD COLUMN note text"})
	second := reportItem(20260901130000, "unique",
		engine.Statement{SQL: "ALTER TABLE widgets ADD COLUMN slug text"},
		engine.Statement{SQL: "CREATE UNIQUE INDEX widgets_code_key ON widgets (code)"},
	)
	r := runReport{
		command: "apply",
		run:     reportRun(godwitv1.RunState_RUN_STATE_FAILED),
		applied: []*godwitv1.RunMigration{ledgerRow("20260901120000_note")},
		plan: planReport{
			live: true, target: "app", rollout: "direct", validated: true, items: []planItem{first, second},
		},
		public: "https://godwit.example.com",
	}
	r.run.Error = "sql: statement 1 of 20260901130000_unique (up): exec: ERROR: could not create unique index"

	return r
}

func render(r runReport) string {
	var b strings.Builder
	writeRunMarkdown(&b, r)

	return b.String()
}

func TestRunReportSaysWhatLandedAndLinksTheRun(t *testing.T) {
	t.Parallel()

	got := render(succeededReport())
	for _, want := range []string{
		"## godwit apply\n",
		"✅ **1 migration applied to `app`.** 1 statement, in 2s.",
		"run [`r1`](https://godwit.example.com/ui/runs/r1)",
		"plan [`p1`](https://godwit.example.com/ui/plans/p1)",
		"commit [`0419cdd`](https://github.com/acme/app/commit/0419cdd1c2f3a4b5c6d7e8f90123456789abcdef)",
		"finished `2026-09-01T12:00:02Z`",
		"\n+ 20260901120000_users  1 statement\n",
		"<details><summary>what this changed on <code>app</code></summary>",
		`+   table "public"."users"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("apply report must carry %q:\n%s", want, got)
		}
	}
}

func TestRunReportWithoutPublicURLLinksNothingItCannotBuild(t *testing.T) {
	t.Parallel()

	r := succeededReport()
	r.public = ""
	r.run.PlanId = ""
	r.run.Source = "somewhere else"
	r.run.FinishedAt = nil
	got := render(r)
	if !strings.Contains(got, "\nrun `r1`\n") {
		t.Fatalf("the run must be named without a link:\n%s", got)
	}
	for _, gone := range []string{"](", "plan `", "commit ", "finished "} {
		if strings.Contains(got, gone) {
			t.Fatalf("nothing the report cannot build may appear, found %q:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "1 statement.") {
		t.Fatalf("a run with no finish time still counts its statements:\n%s", got)
	}
}

func TestRunReportOfNothingApplied(t *testing.T) {
	t.Parallel()

	r := succeededReport()
	r.applied = nil
	r.plan.items[0].skipped = true
	got := render(r)
	if !strings.Contains(got, "✅ **Nothing was applied to `app`.** Every migration the run carried was already on the database.") {
		t.Fatalf("a run that applied nothing must say so:\n%s", got)
	}
	if strings.Contains(got, "```diff") || strings.Contains(got, "what this changed") {
		t.Fatalf("there is nothing to list:\n%s", got)
	}
}

func TestRunReportOfAnAlreadyAppliedMigrationCountsNoStatements(t *testing.T) {
	t.Parallel()

	r := succeededReport()
	r.plan.items[0].alreadyApplied = true
	got := render(r)
	if !strings.Contains(got, "✅ **1 migration applied to `app`.** It took 2s.") {
		t.Fatalf("a migration recorded without executing runs no statements:\n%s", got)
	}
}

func TestRunReportHeldAtTheContractPhase(t *testing.T) {
	t.Parallel()

	got := render(heldReport())
	for _, want := range []string{
		"⏸️ **Expand applied to `app`; the contract phase is held.** 1 of 2 statements ran, in 2s.",
		"+ 20260901120000_people  2 statements, expand then contract phases, written by a directive, contract phase held",
		"`godwit confirm` runs the 1 statement godwit held back:",
		"- `20260901120000_people` statement 1: ALTER TABLE people RENAME COLUMN age TO age_old",
		"The run waits at `awaiting_contract`; confirm it with `godwit confirm` on the pull request.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("held report must carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "what this changed") {
		t.Fatalf("half a migration must not be described as what the database has:\n%s", got)
	}
}

func TestRunReportHeldWithoutAContractStatementSaysOnlyTheState(t *testing.T) {
	t.Parallel()

	r := heldReport()
	r.plan.items[0].Statements = r.plan.items[0].Statements[:1]
	got := render(r)
	if strings.Contains(got, "held back") || strings.Contains(got, "two halves") {
		t.Fatalf("nothing is held, so nothing is listed:\n%s", got)
	}
}

func TestRunReportNamesTheStatementItStoppedAt(t *testing.T) {
	t.Parallel()

	got := render(failedReport())
	for _, want := range []string{
		"❌ **The run stopped part way on `app`.** 1 of 2 migrations applied, in 2s.",
		"+ 20260901120000_note  1 statement\n",
		"  20260901130000_unique  2 statements, stopped at statement 1\n",
		"**It stopped at statement 1 of `20260901130000_unique`.** The 1 statement before it in that migration" +
			" committed and is on the database; the migration itself is not in the target's history.",
		"`statement 1`, runs inside a transaction",
		"```sql\nCREATE UNIQUE INDEX widgets_code_key ON widgets (code);\n```",
		"could not create unique index",
		"If a statement fails, that statement is rolled back and the run stops there;",
		"### `20260901120000_note`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("failure report must carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "### `20260901130000_unique`") {
		t.Fatalf("a migration the target did not take must not be described as applied:\n%s", got)
	}
}

func TestRunReportStopsAtTheFirstStatementOfAMigration(t *testing.T) {
	t.Parallel()

	r := failedReport()
	r.run.Error = "sql: statement 0 of 20260901130000_unique (up): exec: ERROR: boom"
	got := render(r)
	if !strings.Contains(got, "**It stopped at statement 0 of `20260901130000_unique`.**\n") {
		t.Fatalf("nothing committed before statement 0, so nothing is claimed:\n%s", got)
	}
}

func TestRunReportFallsBackToTheLedgerWhenTheErrorNamesNoStatement(t *testing.T) {
	t.Parallel()

	r := failedReport()
	r.run.State = godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION
	r.run.Error = "transient: connection refused"
	got := render(r)
	for _, want := range []string{
		"❌ **The run stopped on `app` and needs attention.**",
		"**It stopped in `20260901130000_unique`**, which is not in the target's history.",
		"  20260901130000_unique  2 statements\n",
		"transient: connection refused",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("failure without a position must carry %q:\n%s", want, got)
		}
	}
}

func TestRunReportOfAFailureBeyondThePlanKeepsTheError(t *testing.T) {
	t.Parallel()

	r := failedReport()
	r.applied = nil
	r.plan.items = nil
	r.run.Error = "sql: statement 9 of 20260901130000_unique (up): exec: ERROR: boom"
	got := render(r)
	if !strings.Contains(got, "❌ **The run stopped part way on `app`.** 0 migrations applied, in 2s.") {
		t.Fatalf("with no plan to count against, the ledger is the whole answer:\n%s", got)
	}
	if strings.Contains(got, "```sql") {
		t.Fatalf("a statement the plan does not carry cannot be shown:\n%s", got)
	}
}

func TestRunReportOfARunStillInFlight(t *testing.T) {
	t.Parallel()

	r := succeededReport()
	r.run.State = godwitv1.RunState_RUN_STATE_RUNNING
	r.run.CreatedAt = nil
	got := render(r)
	if !strings.Contains(got, "ℹ️ **The run is running on `app`.**") {
		t.Fatalf("an unfinished run says its state:\n%s", got)
	}
}

func TestRunReportOfTheContractPhaseLeavesOutTheWait(t *testing.T) {
	t.Parallel()

	r := succeededReport()
	r.command = ""
	r.run.Phase = engine.PhaseContract
	got := render(r)
	if !strings.Contains(got, "## godwit migrate\n") {
		t.Fatalf("with no command given the heading is the run's kind:\n%s", got)
	}
	if !strings.Contains(got, "✅ **1 migration applied to `app`.** 1 statement.") {
		t.Fatalf("the wait for the confirm is not how long the statements took:\n%s", got)
	}
}

func TestRunReportOfARunThatFinishedBeforeItWasCreated(t *testing.T) {
	t.Parallel()

	r := succeededReport()
	r.run.FinishedAt = timestamppb.New(reportStart.Add(-time.Second))
	if got := render(r); !strings.Contains(got, "1 statement.") {
		t.Fatalf("a negative duration is no duration:\n%s", got)
	}
}

func TestRunReportLeavesOutWhatTheRunDidNotCarry(t *testing.T) {
	t.Parallel()

	r := heldReport()
	old := reportItem(20260901110000, "old", engine.Statement{SQL: "SELECT 1", Phase: engine.PhaseContract})
	old.skipped = true
	r.plan.items = append([]planItem{old}, r.plan.items...)
	var b strings.Builder
	writeRunText(&b, r)
	for _, got := range []string{b.String(), render(r)} {
		if strings.Contains(got, "20260901110000_old") {
			t.Fatalf("a migration the run did not execute is not part of its outcome:\n%s", got)
		}
	}
}

func TestRunReportWithNothingToCount(t *testing.T) {
	t.Parallel()

	r := succeededReport()
	r.run.FinishedAt = nil
	r.plan.items = nil
	if got := render(r); !strings.Contains(got, "✅ **1 migration applied to `app`.**\n") {
		t.Fatalf("with neither statements nor a duration the verdict stands alone:\n%s", got)
	}
}

func TestRunReportText(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	writeRunText(&b, failedReport())
	got := b.String()
	for _, want := range []string{
		"The run stopped part way on app. 1 of 2 migrations applied, in 2s.\n",
		"run r1 https://godwit.example.com/ui/runs/r1",
		"+ 20260901120000_note  1 statement\n",
		"  20260901130000_unique  2 statements\n",
		"statement 1, runs inside a transaction\n",
		"    CREATE UNIQUE INDEX widgets_code_key ON widgets (code);\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("text report must carry %q:\n%s", want, got)
		}
	}

	b.Reset()
	writeRunText(&b, heldReport())
	if !strings.Contains(b.String(), "held: 20260901120000_people statement 1: ALTER TABLE people RENAME COLUMN age TO age_old\n") {
		t.Fatalf("the terminal names what is held:\n%s", b.String())
	}
}

func TestRunReportCommand(t *testing.T) {
	stub := &stubService{
		run:     reportRun(godwitv1.RunState_RUN_STATE_SUCCEEDED),
		applied: []*godwitv1.RunMigration{ledgerRow("20260901120000_users")},
		stored:  storedPlanProto(),
	}
	url := startStub(t, stub)
	t.Setenv("GODWIT_PUBLIC_URL", "https://godwit.example.com")

	code, out, errOut := runCLI("run", "report", "r1", "--server", url, "--command", "apply", "--format", "markdown")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.got != "r1" || stub.planGot != "p1" {
		t.Fatalf("the report reads the run and the plan it was bound to: run %q, plan %q", stub.got, stub.planGot)
	}
	if !strings.HasPrefix(out, "## godwit apply\n") ||
		!strings.Contains(out, "run [`r1`](https://godwit.example.com/ui/runs/r1)") {
		t.Fatalf("unexpected report:\n%s", out)
	}

	code, out, errOut = runCLI("run", "report", "r1", "--server", url, "--json")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, `"id":"r1"`) {
		t.Fatalf("--json prints the response:\n%s", out)
	}
}

func TestRunReportWithNoStoredPlan(t *testing.T) {
	t.Parallel()

	run := reportRun(godwitv1.RunState_RUN_STATE_SUCCEEDED)
	run.PlanId = ""
	stub := &stubService{run: run, applied: []*godwitv1.RunMigration{ledgerRow("20260901120000_users")}}
	url := startStub(t, stub)

	code, out, errOut := runCLI("run", "report", "r1", "--server", url, "--format", "markdown")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	if stub.planGot != "" {
		t.Fatalf("an implicit run has no plan to read: %q", stub.planGot)
	}
	if !strings.Contains(out, "✅ **1 migration applied to `app`.** It took 2s.") {
		t.Fatalf("without a plan the ledger is the whole report:\n%s", out)
	}
}

func TestRunReportRefusals(t *testing.T) {
	t.Parallel()

	url := startStub(t, &stubService{run: reportRun(godwitv1.RunState_RUN_STATE_SUCCEEDED), stored: storedPlanProto()})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--format", "json"}, `unknown format "json" (want text or markdown)`},
		{[]string{"--plan-format", "prose"}, `unknown plan format "prose" (want schema or statements)`},
	} {
		code, _, errOut := runCLI(append([]string{"run", "report", "r1", "--server", url}, tc.args...)...)
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Fatalf("code = %d, stderr = %s, want %q", code, errOut, tc.want)
		}
	}

	refused := errors.New("refused")
	for _, stub := range []*stubService{
		{err: refused},
		{run: reportRun(godwitv1.RunState_RUN_STATE_SUCCEEDED), stored: storedPlanProto(), planErr: refused},
	} {
		if code, _, _ := runCLI("run", "report", "r1", "--server", startStub(t, stub)); code == 0 {
			t.Fatalf("a service that refuses must not produce a report")
		}
	}
}

func TestCommitOfIgnoresProvenanceItCannotRead(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"", "db/migrations", "github.com/acme/app@notasha", "acme/app@0419cdd1c2f"} {
		if short, href := commitOf(source); short != "" || href != "" {
			t.Fatalf("commitOf(%q) = %q, %q, want nothing", source, short, href)
		}
	}
	short, href := commitOf("ghe.acme.com/team/app@0419cdd1c2f")
	if short != "0419cdd" || href != "https://ghe.acme.com/team/app/commit/0419cdd1c2f" {
		t.Fatalf("commitOf on an enterprise host = %q, %q", short, href)
	}
}
