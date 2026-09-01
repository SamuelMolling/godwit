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
	})
	if err != nil {
		t.Fatal(err)
	}

	expand, contract := Policies()[RolloutDirect].Split(plans)
	if len(expand) != 3 || len(contract) != 0 {
		t.Fatalf("direct: expand = %d, contract = %d", len(expand), len(contract))
	}

	// The destructive migration and everything after it are held, in order.
	expand, contract = Policies()[RolloutExpandContract].Split(plans)
	if len(expand) != 1 || len(contract) != 2 || contract[0].Migration.Version != 2 {
		t.Fatalf("expand-contract: expand = %d, contract = %+v", len(expand), contract)
	}

	expand, contract = ExpandContract{}.Split(plans[:1])
	if len(expand) != 1 || len(contract) != 0 {
		t.Fatalf("additive only: expand = %d, contract = %d", len(expand), len(contract))
	}
}
