package engine

import (
	"strings"
	"testing"
)

func mig(up string) Migration {
	return Migration{Version: 1, Name: "t", UpSQL: up, DownSQL: "SELECT 1;"}
}

func planUp(t *testing.T, sql string) Plan {
	t.Helper()
	p, err := BuildPlan(mig(sql), DirectionUp)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func hazardCodes(st Statement) []string {
	codes := make([]string, 0, len(st.Hazards))
	for _, h := range st.Hazards {
		codes = append(codes, h.Code)
	}

	return codes
}

func TestBuildPlanSplitsStatements(t *testing.T) {
	t.Parallel()

	p := planUp(t, "CREATE TABLE a (id int);\nCREATE TABLE b (id int);")
	if len(p.Statements) != 2 {
		t.Fatalf("got %d statements, want 2", len(p.Statements))
	}
	if p.Statements[1].SQL != "CREATE TABLE b (id int)" {
		t.Fatalf("bad slice: %q", p.Statements[1].SQL)
	}
	if p.Statements[0].NoTx || p.Statements[0].Hash == "" || p.Statements[0].Verifier != VerifierNone {
		t.Fatalf("plain DDL misclassified: %+v", p.Statements[0])
	}
}

func TestBuildPlanDown(t *testing.T) {
	t.Parallel()

	m := Migration{Version: 1, Name: "t", UpSQL: "SELECT 1;", DownSQL: "DROP TABLE a;"}
	p, err := BuildPlan(m, DirectionDown)
	if err != nil {
		t.Fatal(err)
	}
	if got := hazardCodes(p.Statements[0]); len(got) != 1 || got[0] != "H002" {
		t.Fatalf("hazards = %v, want [H002]", got)
	}
}

func TestBuildPlanClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		sql      string
		noTx     bool
		verifier VerifierKind
		hazards  []string
		schema   string
		index    string
	}{
		{
			name:    "blocking index",
			sql:     "CREATE INDEX idx ON users (email);",
			hazards: []string{"H001"},
		},
		{
			name:     "create index concurrently",
			sql:      "CREATE INDEX CONCURRENTLY idx ON app.users (email);",
			noTx:     true,
			verifier: VerifierCreateIndexConcurrently,
			schema:   "app",
			index:    "idx",
		},
		{
			name:     "drop index concurrently qualified",
			sql:      "DROP INDEX CONCURRENTLY app.idx;",
			noTx:     true,
			verifier: VerifierDropIndexConcurrently,
			schema:   "app",
			index:    "idx",
		},
		{
			name:     "drop index concurrently unqualified",
			sql:      "DROP INDEX CONCURRENTLY idx;",
			noTx:     true,
			verifier: VerifierDropIndexConcurrently,
			index:    "idx",
		},
		{
			name:    "drop index plain",
			sql:     "DROP INDEX idx;",
			hazards: []string{"H009"},
		},
		{
			name: "drop view ignored",
			sql:  "DROP VIEW v;",
		},
		{
			name:    "drop table",
			sql:     "DROP TABLE users;",
			hazards: []string{"H002"},
		},
		{
			name:    "drop column",
			sql:     "ALTER TABLE users DROP COLUMN email;",
			hazards: []string{"H003"},
		},
		{
			name:    "alter column type",
			sql:     "ALTER TABLE users ALTER COLUMN id TYPE bigint;",
			hazards: []string{"H004"},
		},
		{
			name:    "add not null without default",
			sql:     "ALTER TABLE users ADD COLUMN age int NOT NULL;",
			hazards: []string{"H005"},
		},
		{
			name: "add not null with default",
			sql:  "ALTER TABLE users ADD COLUMN age int NOT NULL DEFAULT 0;",
		},
		{
			name: "validate constraint",
			sql:  "ALTER TABLE users VALIDATE CONSTRAINT users_age_check;",
		},
		{
			name:    "add foreign key",
			sql:     "ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id);",
			hazards: []string{"H006"},
		},
		{
			name: "add foreign key not valid",
			sql:  "ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID;",
		},
		{
			name:    "add check",
			sql:     "ALTER TABLE users ADD CHECK (age > 0);",
			hazards: []string{"H006"},
		},
		{
			name: "add check not valid",
			sql:  "ALTER TABLE users ADD CONSTRAINT users_age_check CHECK (age > 0) NOT VALID;",
		},
		{
			name: "add exclusion ignored",
			sql:  "ALTER TABLE users ADD CONSTRAINT users_excl EXCLUDE USING gist (id WITH =);",
		},
		{
			name:    "set not null",
			sql:     "ALTER TABLE users ALTER COLUMN email SET NOT NULL;",
			hazards: []string{"H007"},
		},
		{
			name: "drop not null",
			sql:  "ALTER TABLE users ALTER COLUMN email DROP NOT NULL;",
		},
		{
			name:    "rename table",
			sql:     "ALTER TABLE users RENAME TO accounts;",
			hazards: []string{"H008"},
		},
		{
			name:    "rename column",
			sql:     "ALTER TABLE users RENAME COLUMN email TO mail;",
			hazards: []string{"H008"},
		},
		{
			name: "rename constraint ignored",
			sql:  "ALTER TABLE users RENAME CONSTRAINT a TO b;",
		},
		{
			name:    "add primary key",
			sql:     "ALTER TABLE users ADD PRIMARY KEY (id);",
			hazards: []string{"H010"},
		},
		{
			name: "add primary key using index",
			sql:  "ALTER TABLE users ADD CONSTRAINT users_pkey PRIMARY KEY USING INDEX users_id_uidx;",
		},
		{
			name:    "add unique",
			sql:     "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);",
			hazards: []string{"H010"},
		},
		{
			name: "add unique using index",
			sql:  "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE USING INDEX users_email_uidx;",
		},
		{
			name:     "vacuum",
			sql:      "VACUUM users;",
			noTx:     true,
			verifier: VerifierRerun,
		},
		{
			name:     "refresh matview concurrently",
			sql:      "REFRESH MATERIALIZED VIEW CONCURRENTLY mv;",
			noTx:     true,
			verifier: VerifierRerun,
		},
		{
			name: "refresh matview plain",
			sql:  "REFRESH MATERIALIZED VIEW mv;",
		},
		{
			name:     "reindex concurrently",
			sql:      "REINDEX (CONCURRENTLY) TABLE users;",
			noTx:     true,
			verifier: VerifierRerun,
		},
		{
			name: "reindex plain",
			sql:  "REINDEX TABLE users;",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := planUp(t, tc.sql).Statements[0]
			if st.NoTx != tc.noTx || st.Verifier != tc.verifier {
				t.Fatalf("noTx=%v verifier=%q, want %v %q", st.NoTx, st.Verifier, tc.noTx, tc.verifier)
			}
			if st.IndexSchema != tc.schema || st.IndexName != tc.index {
				t.Fatalf("index = %q.%q, want %q.%q", st.IndexSchema, st.IndexName, tc.schema, tc.index)
			}
			got := hazardCodes(st)
			if len(got) != len(tc.hazards) {
				t.Fatalf("hazards = %v, want %v", got, tc.hazards)
			}
			for i := range got {
				if got[i] != tc.hazards[i] {
					t.Fatalf("hazards = %v, want %v", got, tc.hazards)
				}
			}
		})
	}
}

func TestBuildPlanHazardDetails(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id);": "ADD CONSTRAINT FOREIGN KEY scans the whole table under lock; add it NOT VALID, then VALIDATE CONSTRAINT in a separate statement",
		"ALTER TABLE users ADD CHECK (age > 0);":                                            "ADD CONSTRAINT CHECK scans the whole table under lock; add it NOT VALID, then VALIDATE CONSTRAINT in a separate statement",
		"ALTER TABLE users ALTER COLUMN email SET NOT NULL;":                                "SET NOT NULL on email scans the table under an exclusive lock; add CHECK (email IS NOT NULL) NOT VALID, VALIDATE CONSTRAINT it, then SET NOT NULL is instant on PostgreSQL 12+",
		"ALTER TABLE users RENAME TO accounts;":                                             "RENAME TABLE users breaks application versions still using the old name; add the new table, migrate readers and writers, then drop the old one",
		"ALTER TABLE users RENAME COLUMN email TO mail;":                                    "RENAME COLUMN email breaks application versions still using the old name; add the new column, backfill, migrate readers and writers, then drop the old one",
		"DROP INDEX idx;":                         "DROP INDEX without CONCURRENTLY blocks reads and writes on the table; use DROP INDEX CONCURRENTLY",
		"ALTER TABLE users ADD PRIMARY KEY (id);": "ADD PRIMARY KEY builds its index under an exclusive lock; CREATE UNIQUE INDEX CONCURRENTLY first, then ADD CONSTRAINT ... USING INDEX",
		"ALTER TABLE users ADD UNIQUE (email);":   "ADD UNIQUE builds its index under an exclusive lock; CREATE UNIQUE INDEX CONCURRENTLY first, then ADD CONSTRAINT ... USING INDEX",
	}
	for sql, want := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			st := planUp(t, sql).Statements[0]
			if len(st.Hazards) != 1 || st.Hazards[0].Detail != want {
				t.Fatalf("hazards = %+v, want detail %q", st.Hazards, want)
			}
		})
	}
}

func TestBuildPlanErrors(t *testing.T) {
	t.Parallel()

	if _, err := BuildPlan(mig("CREATE INDEX CONCURRENTLY ON users (email);"), DirectionUp); err == nil ||
		!strings.Contains(err.Error(), "must name the index") {
		t.Fatalf("unnamed concurrent index: err = %v", err)
	}
	if _, err := BuildPlan(mig("NOT SQL AT ALL"), DirectionUp); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("parse error: err = %v", err)
	}
	if _, err := BuildPlan(mig("-- only a comment"), DirectionUp); err == nil || !strings.Contains(err.Error(), "no statements") {
		t.Fatalf("no statements: err = %v", err)
	}
}
