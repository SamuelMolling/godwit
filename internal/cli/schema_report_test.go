package cli

import (
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/engine"
)

func widenAge() planReport {
	return planReport{
		live: true, target: "app", rollout: "direct", validated: true, format: config.PlanFormatSchema,
		items: []planItem{{
			Plan: engine.Plan{
				Migration: engine.Migration{Version: 20260910120000, Name: "widen_age", Checksum: "c"},
				Direction: engine.DirectionUp,
				Statements: []engine.Statement{{
					SQL: "ALTER TABLE public.widgets ALTER COLUMN age TYPE varchar(20)",
					Hazards: []engine.Hazard{{
						Code:   "H004",
						Detail: "ALTER COLUMN TYPE rewrites the table under an exclusive lock",
						Recipe: "-- godwit: change-type public.widgets.age varchar(20)",
						Object: "public.widgets", Attribute: "age",
					}},
				}},
			},
			changes: []engine.ObjectChange{{
				Op: engine.OpUpdate, Kind: engine.KindTable, Schema: "public", Name: "widgets", Unchanged: 4,
				Attrs: []engine.AttrChange{
					{Op: engine.OpUpdate, Name: "age", Old: []string{"integer"}, New: []string{"character varying"}},
					{Op: engine.OpCreate, Name: "age_old", New: []string{"integer", "NULL"}},
				},
			}},
		}},
	}
}

func TestSchemaTextPutsTheHazardOnTheAttributeThatCausesIt(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanText(&b, widenAge())
	want := "godwit will perform the following actions:\n" +
		"\n  # 20260910120000_widen_age\n" +
		"  ~ table \"public\".\"widgets\" {\n" +
		"      ~ age     = integer -> character varying" +
		" # ALTER COLUMN TYPE rewrites the table under an exclusive lock (H004)\n" +
		"      + age_old = integer NULL\n" +
		"        # (4 unchanged attributes hidden)\n" +
		"    }\n" +
		"    recipe for H004:\n      -- godwit: change-type public.widgets.age varchar(20)\n"
	if !strings.Contains(b.String(), want) {
		t.Fatalf("missing %q in:\n%s", want, b.String())
	}
	if !strings.HasSuffix(b.String(), "Plan: 0 to add, 1 to change, 0 to destroy.\n") {
		t.Fatalf("footer counts objects:\n%s", b.String())
	}
}

func TestSchemaMarkdownFencesTheBlockAndCollapsesTheRecipe(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writePlanMarkdown(&b, widenAge())
	for _, want := range []string{
		"\n```diff\n~ table \"public\".\"widgets\" {\n" +
			"    ~ age     = integer -> character varying" +
			" # ALTER COLUMN TYPE rewrites the table under an exclusive lock (H004)\n" +
			"    + age_old = integer NULL\n      # (4 unchanged attributes hidden)\n  }\n```\n",
		"<details><summary>H004 recipe</summary>\n\n```sql\n-- godwit: change-type public.widgets.age varchar(20)\n```",
		"\nPlan: 0 to add, 1 to change, 0 to destroy.\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "```sql\nALTER TABLE public.widgets ALTER COLUMN age TYPE") {
		t.Fatalf("the schema format does not list the statements:\n%s", b.String())
	}
}

func TestStatementsFormatIgnoresTheSchemaDelta(t *testing.T) {
	t.Parallel()
	r := widenAge()
	r.format = config.PlanFormatStatements
	var b strings.Builder
	writePlanText(&b, r)
	if !strings.Contains(b.String(), "+ 20260910120000_widen_age  1 statement\n  statement 0, runs inside a transaction\n"+
		"      ALTER TABLE public.widgets ALTER COLUMN age TYPE varchar(20);\n") {
		t.Fatalf("out:\n%s", b.String())
	}
	if strings.Contains(b.String(), actionsHeading) || !strings.HasSuffix(b.String(),
		"Plan: 1 to apply, 0 to revert, 1 hazard(s) to acknowledge\n") {
		t.Fatalf("out:\n%s", b.String())
	}
}

func TestSchemaBlocksCoverEveryObjectKind(t *testing.T) {
	t.Parallel()
	r := widenAge()
	r.items[0].Statements = nil
	r.items[0].changes = []engine.ObjectChange{
		{Op: engine.OpDestroy, Kind: engine.KindTable, Schema: "public", Name: "old", Attrs: []engine.AttrChange{
			{Op: engine.OpDestroy, Name: "id", Old: []string{"bigint", "NOT NULL"}},
		}},
		{Op: engine.OpCreate, Kind: engine.KindIndex, Schema: "public", Name: "widgets_kind_idx", Attrs: []engine.AttrChange{
			{Op: engine.OpCreate, Name: "definition", New: []string{"CREATE INDEX widgets_kind_idx ON public.widgets USING btree (kind)"}},
		}},
		{Op: engine.OpUpdate, Kind: engine.KindMatView, Name: "mv", Attrs: []engine.AttrChange{
			{Op: engine.OpUpdate, Name: "definition", Old: []string{"a"}, New: []string{"b"}},
		}},
		{Op: engine.OpCreate, Kind: engine.KindView, Schema: "public", Name: "stats", Attrs: []engine.AttrChange{
			{Op: engine.OpCreate, Name: "definition", New: []string{"5d41402abc4b2a76b9719d911017c592"}},
		}},
	}
	var b strings.Builder
	writePlanText(&b, r)
	for _, want := range []string{
		"  - table \"public\".\"old\" {\n      - id = bigint NOT NULL\n    }\n",
		"  + index \"public\".\"widgets_kind_idx\" {\n      + definition = CREATE INDEX widgets_kind_idx ON public.widgets USING btree (kind)\n    }\n",
		// A snapshot records a view by the hash of its body, which is not what a reader wants to be shown.
		"  ~ materialized view \"mv\" {\n      ~ definition = (changed)\n    }\n",
		"  + view \"public\".\"stats\"\n",
		"Plan: 2 to add, 1 to change, 1 to destroy.\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
}

func TestSchemaKeepsHazardsItCannotPlaceOnAnAttribute(t *testing.T) {
	t.Parallel()
	r := widenAge()
	r.items[0].Statements = []engine.Statement{{
		SQL: "INSERT INTO audit VALUES (1)",
		Hazards: []engine.Hazard{
			{Code: "H002", Detail: "DROP TABLE is destructive", Object: "audit"},
			{Code: "H007", Detail: "SET NOT NULL on kind scans the table; add CHECK", Object: "widgets", Attribute: "kind"},
			{Code: "H003", Detail: "DROP COLUMN is destructive", Attribute: "age"},
		},
	}}
	r.items[0].changes[0].Name = "widgets"
	var b strings.Builder
	writePlanText(&b, r)
	for _, want := range []string{
		"  # DROP COLUMN is destructive (H003)\n",
		"  # SET NOT NULL on kind scans the table (H007)\n  ~ table \"public\".\"widgets\" {",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
	if !strings.Contains(b.String(), "  # DROP TABLE is destructive (H002)\n") {
		t.Fatalf("a hazard about an object outside the delta is still reported:\n%s", b.String())
	}
}

func TestSchemaSaysWhyOneMigrationHasNoBlock(t *testing.T) {
	t.Parallel()
	r := widenAge()
	seed := planItem{Plan: engine.Plan{
		Migration:  engine.Migration{Version: 20260910130000, Name: "seed"},
		Direction:  engine.DirectionUp,
		Statements: []engine.Statement{{SQL: "INSERT INTO widgets (id) VALUES (1)", Opaque: "has DML"}},
	}}
	quiet := planItem{Plan: engine.Plan{
		Migration:  engine.Migration{Version: 20260910140000, Name: "grant"},
		Direction:  engine.DirectionUp,
		Statements: []engine.Statement{{SQL: "GRANT SELECT ON widgets TO reader"}},
	}}
	r.items = append(r.items, seed, quiet)
	var b strings.Builder
	writePlanText(&b, r)
	for _, want := range []string{
		"  # a schema snapshot cannot see what this does (has DML); the statements it runs are below\n",
		"  # no schema change was recorded for this one; the statements it runs are below\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, b.String())
		}
	}
	var md strings.Builder
	writePlanMarkdown(&md, r)
	if !strings.Contains(md.String(), "\n# a schema snapshot cannot see what this does (has DML); the statements it runs are below\n") {
		t.Fatalf("markdown:\n%s", md.String())
	}
}

func TestSchemaHeadingCarriesWhatTheSummaryUsedTo(t *testing.T) {
	t.Parallel()
	r := widenAge()
	r.items[0].changes = nil
	r.items[0].expanded = true
	r.items[0].alreadyApplied = true
	r.items[0].effect = "+ column public.widgets.age_old integer null=YES default=<none>"
	r.items = append(r.items, widenAge().items[0])
	var b strings.Builder
	writePlanText(&b, r)
	want := "  # 20260910120000_widen_age  (written by a directive; already on the database, so the run records it" +
		" without executing)\n  + column public.widgets.age_old integer null=YES default=<none>\n"
	if !strings.Contains(b.String(), want) {
		t.Fatalf("missing %q in:\n%s", want, b.String())
	}
	if strings.Contains(b.String(), "no schema change was recorded") {
		t.Fatalf("the effect lines already say what it did:\n%s", b.String())
	}

	r.items[0].alreadyApplied, r.items[0].note = false, "has DML, must execute"
	b.Reset()
	writePlanText(&b, r)
	if !strings.Contains(b.String(), "  # 20260910120000_widen_age  (written by a directive; has DML, must execute)\n") {
		t.Fatalf("out:\n%s", b.String())
	}
}

func TestPlanFormatIsRefusedWhenItIsNeitherOfTheTwo(t *testing.T) {
	t.Parallel()
	code, _, errOut := runCLI("plan", "--dir", goodMigs(t), "--plan-format", "diagram")
	if code == 0 || !strings.Contains(errOut, `unknown plan format "diagram" (want schema or statements)`) {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func TestPlanCarriesTheSchemaChangeOverTheWire(t *testing.T) {
	t.Parallel()
	url := startStub(t, &stubService{plan: &godwitv1.PlanRunResponse{
		Target: "app", Rollout: "direct", Validated: true,
		Migrations: []*godwitv1.PlannedMigration{{
			Version: 20260910120000, Name: "widen_age", Checksum: "c",
			Statements: []*godwitv1.PlannedStatement{{
				Sql: "ALTER TABLE public.widgets ALTER COLUMN age TYPE varchar(20)",
				Hazards: []*godwitv1.PlannedHazard{{
					Code:   "H004",
					Detail: "ALTER COLUMN TYPE rewrites the table under an exclusive lock",
					Object: "public.widgets", Attribute: "age",
				}},
			}},
			Changes: []*godwitv1.SchemaChange{{
				Op: engine.OpUpdate, Kind: engine.KindTable, Schema: "public", Name: "widgets", Unchanged: 4,
				Attributes: []*godwitv1.SchemaAttribute{
					{Op: engine.OpUpdate, Name: "age", Old: []string{"integer"}, New: []string{"character varying"}},
				},
			}},
		}},
	}})

	code, out, errOut := runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "  ~ table \"public\".\"widgets\" {\n      ~ age = integer -> character varying" +
		" # ALTER COLUMN TYPE rewrites the table under an exclusive lock (H004)\n" +
		"        # (4 unchanged attributes hidden)\n    }\n"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in:\n%s", want, out)
	}

	code, out, _ = runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t), "--plan-format", "statements")
	if code != 0 || strings.Contains(out, "~ table") ||
		!strings.Contains(out, "ALTER TABLE public.widgets ALTER COLUMN age TYPE varchar(20);") {
		t.Fatalf("code = %d, out = %s", code, out)
	}
}

func TestSchemaMarkdownKeepsALooseHazardAndSkipsWhatDoesNotRun(t *testing.T) {
	t.Parallel()
	r := widenAge()
	r.items[0].Statements[0].Hazards[0].Object = "elsewhere"
	r.items[0].Statements[0].Hazards[0].Recipe = ""
	r.items = append(r.items, planItem{
		skipped: true,
		changes: []engine.ObjectChange{{Op: engine.OpCreate, Kind: engine.KindTable, Name: "not_run"}},
		Plan: engine.Plan{
			Migration: engine.Migration{Version: 20260910110000, Name: "old"}, Direction: engine.DirectionUp,
		},
	})
	var b strings.Builder
	writePlanMarkdown(&b, r)
	if !strings.Contains(b.String(), "```diff\n# ALTER COLUMN TYPE rewrites the table under an exclusive lock (H004)\n~ table") {
		t.Fatalf("out:\n%s", b.String())
	}
	if strings.Contains(b.String(), "recipe</summary>") {
		t.Fatalf("a hazard with no recipe has no block to collapse:\n%s", b.String())
	}
	r.items[0].Statements = append(r.items[0].Statements, r.items[0].Statements[0])
	r.items[0].Statements[1].Hazards[0].Recipe = "-- second"
	b.Reset()
	writePlanMarkdown(&b, r)
	if strings.Count(b.String(), "<details><summary>H004 recipe</summary>") != 1 {
		t.Fatalf("one recipe per hazard code, not per statement:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "\nPlan: 0 to add, 1 to change, 0 to destroy.\n") {
		t.Fatalf("a skipped migration changes nothing this run does:\n%s", b.String())
	}
}
