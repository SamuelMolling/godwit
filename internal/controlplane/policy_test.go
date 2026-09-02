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
