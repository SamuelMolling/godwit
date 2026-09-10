package cli

import (
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"

	"github.com/SamuelMolling/godwit/internal/engine"
)

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
					{Op: engine.OpUpdate, Name: "age", Old: []string{"integer"}, New: []string{"character varying(20)"}},
				},
			}},
		}},
	}})

	code, out, errOut := runCLI("plan", "--server", url, "--target", "app", "--dir", goodMigs(t))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "  ~ table \"public\".\"widgets\" {\n      ~ age = integer -> character varying(20)" +
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
