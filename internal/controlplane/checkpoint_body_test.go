package controlplane

import (
	"strings"
	"testing"
)

const pkPair = `CREATE TABLE "public"."t" (
	"id" bigint NOT NULL
);
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id);
ALTER TABLE "public"."t" ADD CONSTRAINT "t_pkey" PRIMARY KEY USING INDEX "t_pkey";`

func TestFoldStatementsFoldsThePair(t *testing.T) {
	t.Parallel()
	got, err := foldStatements(pkPair)
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE TABLE "public"."t" (
	"id" bigint NOT NULL,
	CONSTRAINT "t_pkey" PRIMARY KEY ("id")
);`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFoldStatementsFoldsAUniqueConstraint(t *testing.T) {
	t.Parallel()
	got, err := foldStatements(`CREATE TABLE public.t (a int, b int);
CREATE UNIQUE INDEX t_ab ON public.t USING btree (a, b);
ALTER TABLE public.t ADD CONSTRAINT t_ab UNIQUE USING INDEX t_ab`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `CONSTRAINT "t_ab" UNIQUE ("a", "b")`) || strings.Contains(got, "USING INDEX") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestFoldStatementsFoldsAValidatedConstraint(t *testing.T) {
	t.Parallel()
	got, err := foldStatements(`CREATE TABLE "public"."a" (
	"id" bigint NOT NULL
);
ALTER TABLE "public"."b" ADD CONSTRAINT "b_a_fkey" FOREIGN KEY ("a_id") REFERENCES "public"."a" ("id") NOT VALID;
ALTER TABLE "public"."b" VALIDATE CONSTRAINT "b_a_fkey";`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "NOT VALID") || strings.Contains(got, "VALIDATE CONSTRAINT") ||
		!strings.Contains(got, `REFERENCES "public"."a" ("id");`) {
		t.Fatalf("got:\n%s", got)
	}
}

func TestFoldStatementsRefusesAnUnparseableBody(t *testing.T) {
	t.Parallel()
	if _, err := foldStatements("NOT SQL"); err == nil ||
		!strings.Contains(err.Error(), "parse the rendered schema") {
		t.Fatalf("err = %v", err)
	}
}

// Everything the fold cannot reproduce as a table constraint is left exactly as the generator wrote it.
func TestFoldStatementsLeavesWhatItCannotReproduce(t *testing.T) {
	t.Parallel()
	for name, ddl := range map[string]string{
		"no constraint at all": `CREATE TABLE public.t (id int);
ALTER TABLE public.t ADD COLUMN note text;`,
		"constraint without an index": `CREATE TABLE public.t (id int);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY (id);`,
		"two commands at once": `CREATE TABLE public.t (id int);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey, ADD COLUMN note text;`,
		"deferrable": `CREATE TABLE public.t (id int);
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey DEFERRABLE;`,
		"index the body does not build": `CREATE TABLE public.t (id int);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;`,
		"table the body does not create": `CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;`,
		"a partition": `CREATE TABLE public.t PARTITION OF public.p FOR VALUES IN (1);
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;`,
		"a partial index": `CREATE TABLE public.t (id int);
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id) WHERE id > 0;
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;`,
		"an expression index": `CREATE TABLE public.t (note text);
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (lower(note));
ALTER TABLE public.t ADD CONSTRAINT t_pkey UNIQUE USING INDEX t_pkey;`,
		"a descending index": `CREATE TABLE public.t (id int);
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id DESC);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;`,
		"a table with no closing parenthesis": `CREATE TABLE public.t (id int) TABLESPACE fast;
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;`,
		"a table with no columns": `CREATE TABLE public.t ();
CREATE UNIQUE INDEX t_pkey ON public.t USING btree (id);
ALTER TABLE public.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;`,
		"a validation with nothing to validate": `CREATE TABLE public.t (id int);
ALTER TABLE public.t VALIDATE CONSTRAINT t_chk;`,
		"a constraint the generator did not write NOT VALID last": `CREATE TABLE public.t (id int);
ALTER TABLE public.t ADD CONSTRAINT t_chk CHECK (id > 0) not valid;
ALTER TABLE public.t VALIDATE CONSTRAINT t_chk;`,
	} {
		got, err := foldStatements(ddl)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Count(got, ";") != strings.Count(ddl, ";") || strings.Contains(got, "CONSTRAINT \"") {
			t.Fatalf("%s: the body must be left alone:\n%s", name, got)
		}
	}
}
