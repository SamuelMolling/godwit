package controlplane

import (
	"testing"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func TestPolicies(t *testing.T) {
	t.Parallel()

	plans, err := buildPlans([]engine.Migration{
		{Version: 1, Name: "a", UpSQL: "CREATE TABLE a (id int);", DownSQL: "DROP TABLE a;"},
		{Version: 2, Name: "b", UpSQL: "ALTER TABLE a DROP COLUMN id;", DownSQL: "SELECT 1;"},
		{Version: 3, Name: "c", UpSQL: "CREATE TABLE c (id int);", DownSQL: "DROP TABLE c;"},
	}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}

	expand, contract := Policies()[RolloutDirect].Split(plans)
	if len(expand) != 3 || len(contract) != 0 {
		t.Fatalf("direct: expand = %d, contract = %d", len(expand), len(contract))
	}

	expand, contract = Policies()[RolloutExpandContract].Split(plans)
	if len(expand) != 1 || len(contract) != 2 || contract[0].Migration.Version != 2 {
		t.Fatalf("expand-contract: expand = %d, contract = %+v", len(expand), contract)
	}

	expand, contract = ExpandContract{}.Split(plans[:1])
	if len(expand) != 1 || len(contract) != 0 {
		t.Fatalf("additive only: expand = %d, contract = %d", len(expand), len(contract))
	}
}

func TestExpandContractHazards(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code     string
		sql      string
		contract bool
	}{
		{"H001", "CREATE INDEX a_id ON a (id);", false},
		{"H002", "DROP TABLE a;", true},
		{"H003", "ALTER TABLE a DROP COLUMN id;", true},
		{"H004", "ALTER TABLE a ALTER COLUMN id TYPE bigint;", false},
		{"H005", "ALTER TABLE a ADD COLUMN b int NOT NULL;", false},
		{"H006", "ALTER TABLE a ADD CONSTRAINT a_chk CHECK (id > 0);", false},
		{"H007", "ALTER TABLE a ALTER COLUMN id SET NOT NULL;", false},
		{"H008", "ALTER TABLE a RENAME TO b;", true},
		{"H008", "ALTER TABLE a RENAME COLUMN id TO ident;", true},
		{"H009", "DROP INDEX a_id;", false},
		{"H010", "ALTER TABLE a ADD PRIMARY KEY (id);", false},
	}

	for _, tc := range cases {
		t.Run(tc.code+" "+tc.sql, func(t *testing.T) {
			t.Parallel()

			plans, err := buildPlans([]engine.Migration{
				{Version: 1, Name: "a", UpSQL: "CREATE TABLE a (id int);", DownSQL: "DROP TABLE a;"},
				{Version: 2, Name: "b", UpSQL: tc.sql, DownSQL: "SELECT 1;"},
			}, engine.DirectionUp)
			if err != nil {
				t.Fatal(err)
			}

			if len(plans[1].Statements[0].Hazards) == 0 || plans[1].Statements[0].Hazards[0].Code != tc.code {
				t.Fatalf("hazards = %+v, want %s", plans[1].Statements[0].Hazards, tc.code)
			}

			expand, contract := ExpandContract{}.Split(plans)
			if got := len(contract) == 1; got != tc.contract {
				t.Fatalf("contract = %v, want %v (expand = %d)", got, tc.contract, len(expand))
			}
		})
	}
}

func phasedPlans(t *testing.T, at int) []engine.Plan {
	t.Helper()
	plans, err := buildPlans([]engine.Migration{
		{Version: 1, Name: "a", UpSQL: "CREATE TABLE a (id int);", DownSQL: "DROP TABLE a;"},
		{
			Version: 2, Name: "b", DownSQL: "SELECT 1;",
			UpSQL: "ALTER TABLE a ADD COLUMN b int;\nALTER TABLE a ADD COLUMN c int;\nSELECT 1;",
		},
		{Version: 3, Name: "c", UpSQL: "CREATE TABLE c (id int);", DownSQL: "DROP TABLE c;"},
	}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	for i := at; i < len(plans[1].Statements); i++ {
		plans[1].Statements[i].Phase = engine.PhaseContract
	}

	return plans
}

func TestExpandContractHoldsInsideOnePlan(t *testing.T) {
	t.Parallel()

	plans := phasedPlans(t, 2)
	expand, contract := ExpandContract{}.Split(plans)

	if len(expand) != 2 || !expand[1].Held() || expand[1].HoldFrom != 2 {
		t.Fatalf("expand = %d plans, hold from %d", len(expand), expand[len(expand)-1].HoldFrom)
	}
	if len(contract) != 2 || contract[0].Migration.Version != 2 || contract[0].HoldFrom != 0 {
		t.Fatalf("contract = %+v", contract)
	}
	if plans[1].HoldFrom != 0 || expand[0].Migration.Version != 1 {
		t.Fatalf("split must not touch the plans it was given: %+v", plans)
	}
	if n := HeldStatements(expand, contract); n != 2 {
		t.Fatalf("held statements = %d, want 2", n)
	}

	expand, contract = Direct{}.Split(phasedPlans(t, 2))
	if len(expand) != 3 || contract != nil || expand[1].Held() {
		t.Fatalf("direct must ignore phases: expand = %d, contract = %d", len(expand), len(contract))
	}
}

func TestExpandContractHoldsWholePlanFromStatementZero(t *testing.T) {
	t.Parallel()

	plans := phasedPlans(t, 0)
	expand, contract := ExpandContract{}.Split(plans)

	if len(expand) != 1 || expand[0].Held() {
		t.Fatalf("expand = %+v", expand)
	}
	if len(contract) != 2 || contract[0].HoldFrom != 0 {
		t.Fatalf("contract = %+v", contract)
	}
	if n := HeldStatements(expand, contract); n != 4 {
		t.Fatalf("held statements = %d, want 4", n)
	}
}
