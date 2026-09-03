package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func render(d Directive) string {
	parts := append([]string{d.Op}, d.Args...)
	names := make([]string, 0, len(d.Opts))
	for k := range d.Opts {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		parts = append(parts, k+"="+d.Opts[k])
	}

	return strings.Join(parts, "|")
}

func TestParseDirectivesGrammar(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"-- godwit: change-type users.age bigint using='age::bigint' batch=5000 pause=100ms":              "change-type|users.age|bigint|batch=5000|pause=100ms|using=age::bigint",
		"-- godwit: change-type users.age bigint keep-old=false not-null":                                 "change-type|users.age|bigint|keep-old=false|not-null=true",
		"-- godwit: change-type app.users.age numeric(10, 2)":                                             "change-type|app.users.age|numeric(10, 2)",
		"-- godwit: add-not-null users.email":                                                             "add-not-null|users.email",
		"-- godwit: add-column users.age int default='0' not-null":                                        "add-column|users.age|int|default=0|not-null=true",
		"-- godwit: add-index users (email) where='deleted_at IS NULL'":                                   "add-index|users|(email)|where=deleted_at IS NULL",
		"-- godwit: add-index app.users (lower(email), id) name=i using=btree unique":                     "add-index|app.users|(lower(email), id)|name=i|unique=true|using=btree",
		"-- godwit: drop-index app.users_email_idx":                                                       "drop-index|app.users_email_idx",
		"-- godwit: add-fk orders.user_id -> users.id":                                                    "add-fk|orders.user_id|->|users.id",
		"-- godwit: add-fk orders.user_id -> users.id name=fk on-delete=set-null":                         "add-fk|orders.user_id|->|users.id|name=fk|on-delete=set-null",
		"-- godwit: add-check users users_age_check 'age > 0'":                                            "add-check|users|users_age_check|age > 0",
		"-- godwit: drop-column users.age_old":                                                            "drop-column|users.age_old",
		"-- godwit: backfill users set='age_new = age::bigint' where='age_new IS NULL' key=id batch=5000": "backfill|users|batch=5000|key=id|set=age_new = age::bigint|where=age_new IS NULL",
		"--godwit:revert":                                "revert",
		"   \t-- godwit:   drop-column users.a   ":       "drop-column|users.a",
		"-- godwit: add-check users c 'name <> ''x'''":   "add-check|users|c|name <> 'x'",
		"-- godwit: backfill users set='a = ''x'''":      "backfill|users|set=a = 'x'",
		"-- godwit: add-index users (\"user\", id)":      "add-index|users|(\"user\", id)",
		"-- godwit: add-index users (coalesce(a, 'x'))":  "add-index|users|(coalesce(a, 'x'))",
		"-- godwit: backfill users set='a = b' pause=0s": "backfill|users|pause=0s|set=a = b",
	}
	for sql, want := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			ds, err := ParseDirectives(sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(ds) != 1 {
				t.Fatalf("got %d directives", len(ds))
			}
			if got := render(ds[0]); got != want {
				t.Fatalf("parsed %q, want %q", got, want)
			}
			if ds[0].Line != 1 || ds[0].Text != strings.TrimSpace(sql) {
				t.Fatalf("line %d text %q", ds[0].Line, ds[0].Text)
			}
		})
	}
}

func TestParseDirectivesErrors(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"-- godwit:": "missing operation",
		"-- godwit: rename-column users.a users.b":          `unknown operation "rename-column"`,
		"-- godwit: drop-column":                            "drop-column takes 1 argument(s), got 0",
		"-- godwit: drop-column users":                      `"users" must have the form <table>.<column>`,
		"-- godwit: drop-column a.b.c.d":                    `"a.b.c.d" must have the form <table>.<column>`,
		"-- godwit: drop-column users.*":                    `"users.*" is not a name`,
		"-- godwit: drop-column 7":                          `"7" is not a name`,
		"-- godwit: drop-column users.a extra":              `drop-column: unexpected argument "extra"`,
		"-- godwit: drop-column users.a batch=1":            `drop-column has no option "batch"; known: `,
		"-- godwit: change-type users.a bigint batch=nope":  `change-type option batch: "nope" is not a positive integer`,
		"-- godwit: change-type users.a bigint batch=0":     `change-type option batch: "0" is not a positive integer`,
		"-- godwit: change-type users.a bigint pause=soon":  `change-type option pause: "soon" is not a duration such as 100ms`,
		"-- godwit: change-type users.a bigint pause=-1s":   `change-type option pause: "-1s" is not a duration such as 100ms`,
		"-- godwit: change-type users.a bigint keep-old=no": `change-type option keep-old: "no" is not true or false`,
		"-- godwit: change-type users.a bigint key=a.b":     `change-type option key: "a.b" must have a single name`,
		"-- godwit: change-type users.a bigint using='('":   "change-type option using: not an expression",
		"-- godwit: change-type users.a 'not a type'":       "change-type argument 2: not a type",
		"-- godwit: add-index users email":                  `"email" is not a parenthesised column list`,
		"-- godwit: add-index users (order by)":             "add-index argument 2: not a column list",
		"-- godwit: add-index users (a) where='('":          "add-index option where: not a condition",
		"-- godwit: backfill users":                         "backfill requires set=",
		"-- godwit: backfill users set='a ='":               "backfill option set: not an assignment list",
		"-- godwit: add-fk orders.a => users.b":             `add-fk argument 2: expected ->, got "=>"`,
		"-- godwit: add-fk orders.a -> users.b on-delete=x": `add-fk option on-delete: "x" is not one of cascade`,
		"-- godwit: add-check users c 'age >'":              "add-check argument 3: not an expression",
		"-- godwit: drop-column users.'a":                   "unterminated quoted value",
		"-- godwit: add-index users (a":                     "unterminated (",
		"-- godwit: add-index users (a, 'x":                 "unterminated quoted value",
		"-- godwit: drop-index a.b.c":                       `"a.b.c" must have the form <table> or <schema>.<table>`,
		"-- godwit: drop-column a,b":                        `"a,b" is not a name`,
		"-- godwit: drop-index 'from'":                      "not a name: syntax error",
		"-- godwit: drop-column users.a 'x = 1'":            `drop-column: unexpected argument "x = 1"`,
		"SELECT 1; -- godwit: drop-column users.a":          "a directive must start its own line",
	}
	for sql, want := range cases {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			_, err := ParseDirectives(sql)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want %q", err, want)
			}
			var de *DirectiveError
			if !errors.As(err, &de) || de.Line != 1 || !strings.HasPrefix(err.Error(), "line 1: godwit directive: ") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestParseDirectivesPositions(t *testing.T) {
	t.Parallel()

	sql := `-- a plain comment
/* block
   -- godwit: drop-column users.a
   nested /* deeper */ still */
INSERT INTO t VALUES ('-- godwit: drop-column users.b', "-- godwit: x");
CREATE FUNCTION f() RETURNS int AS $body$
-- godwit: drop-column users.c
SELECT 1 $body$ LANGUAGE sql;
SELECT $1, $$ -- godwit: drop-column users.d $$;
-- godwit: drop-column users.e
ALTER TABLE users DROP COLUMN e;
-- godwit: add-not-null users.f`
	ds, err := ParseDirectives(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 {
		t.Fatalf("got %d directives: %+v", len(ds), ds)
	}
	if ds[0].Op != "drop-column" || ds[0].Args[0] != "users.e" || ds[0].Line != 10 {
		t.Fatalf("first = %+v", ds[0])
	}
	if ds[1].Op != "add-not-null" || ds[1].Line != 12 {
		t.Fatalf("last = %+v", ds[1])
	}
}

func TestParseDirectivesUnterminated(t *testing.T) {
	t.Parallel()

	cases := map[string]int{"/* open": 0, "SELECT 'open": 0, "SELECT $$open": 0, "SELECT $": 1, "SELECT 'a''b' -": 1}
	if ds, err := ParseDirectives("SELECT 1 -"); err != nil || len(ds) != 0 {
		t.Fatalf("trailing dash: %v %+v", err, ds)
	}
	for sql, want := range cases {
		ds, err := ParseDirectives(sql + "\n-- godwit: drop-column users.a")
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if len(ds) != want {
			t.Fatalf("%q: got %d directives, want %d", sql, len(ds), want)
		}
	}
}

func TestValidateDirectiveDirect(t *testing.T) {
	t.Parallel()

	if err := ValidateDirective(Directive{Op: "nope"}); err == nil {
		t.Fatal("want unknown op error")
	}
	if err := ValidateDirective(Directive{Op: "drop-column"}); err == nil || !strings.Contains(err.Error(), "got 0") {
		t.Fatalf("err = %v", err)
	}
	if err := ValidateDirective(Directive{Op: DirectiveRevert}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectiveErrorFile(t *testing.T) {
	t.Parallel()

	e := &DirectiveError{File: "a.up.sql", Line: 3, Msg: "boom"}
	if e.Error() != "a.up.sql:3: godwit directive: boom" {
		t.Fatalf("error = %q", e.Error())
	}
}

func TestRecipeHintsAreValidDirectives(t *testing.T) {
	t.Parallel()

	for sql := range recipeCases() {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			recipe := planUp(t, sql).Statements[0].Hazards[0].Recipe
			line, _, _ := strings.Cut(recipe, "\n")
			if !strings.Contains(line, "-- "+DirectiveMarker) {
				return
			}
			_, hint, _ := strings.Cut(line, "-- "+DirectiveMarker)
			ds, err := ParseDirectives("-- " + DirectiveMarker + hint)
			if err != nil {
				t.Fatalf("hint %q: %v", hint, err)
			}
			if len(ds) != 1 {
				t.Fatalf("hint %q parsed into %d directives", hint, len(ds))
			}
		})
	}
}

func TestLoadDirDirectives(t *testing.T) {
	t.Parallel()

	dir := writeFiles(t, map[string]string{
		"20260901120000_age.up.sql":    "-- godwit: change-type users.age bigint\nALTER TABLE users ADD COLUMN note text;",
		"20260901120000_age.down.sql":  "  -- godwit: revert\n",
		"20260901120100_note.up.sql":   "ALTER TABLE users ADD COLUMN note2 text;",
		"20260901120100_note.down.sql": "ALTER TABLE users DROP COLUMN note2;",
	})
	migs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migs[0].Directives) != 1 || migs[0].Directives[0].Op != "change-type" || !migs[0].RevertDirective {
		t.Fatalf("first = %+v", migs[0])
	}
	if len(migs[1].Directives) != 0 || migs[1].RevertDirective {
		t.Fatalf("second = %+v", migs[1])
	}
}

func TestLoadDirDirectiveErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			"up syntax",
			map[string]string{"20260901120000_a.up.sql": "-- godwit: nope\nSELECT 1;", "20260901120000_a.down.sql": "SELECT 1;"},
			`20260901120000_a.up.sql:1: godwit directive: unknown operation "nope"`,
		},
		{
			"down syntax",
			map[string]string{"20260901120000_a.up.sql": "SELECT 1;", "20260901120000_a.down.sql": "SELECT 1;\n-- godwit: nope"},
			`20260901120000_a.down.sql:2: godwit directive: unknown operation "nope"`,
		},
		{
			"revert in up",
			map[string]string{"20260901120000_a.up.sql": "-- godwit: revert\nSELECT 1;", "20260901120000_a.down.sql": "SELECT 1;"},
			"20260901120000_a.up.sql:1: godwit directive: the revert sentinel belongs in the .down.sql",
		},
		{
			"directive in down",
			map[string]string{"20260901120000_a.up.sql": "SELECT 1;", "20260901120000_a.down.sql": "-- godwit: drop-column users.a\nSELECT 1;"},
			"20260901120000_a.down.sql:1: godwit directive: a .down.sql carries hand-written SQL or the lone",
		},
		{
			"revert with sql",
			map[string]string{"20260901120000_a.up.sql": "SELECT 1;", "20260901120000_a.down.sql": "-- godwit: revert\nSELECT 1;"},
			"20260901120000_a.down.sql:1: godwit directive: a .down.sql carries hand-written SQL or the lone",
		},
		{
			"repeatable up",
			map[string]string{"R__v.up.sql": "-- godwit: drop-column users.a\nSELECT 1;", "R__v.down.sql": "SELECT 1;"},
			"R__v.up.sql:1: godwit directive: directives are not supported in a repeatable migration",
		},
		{
			"repeatable down",
			map[string]string{"R__v.up.sql": "SELECT 1;", "R__v.down.sql": "-- godwit: revert\n"},
			"R__v.down.sql:1: godwit directive: directives are not supported in a repeatable migration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadDir(writeFiles(t, tc.files))
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want prefix %q", err, tc.want)
			}
		})
	}
}

func TestDirectiveOptionNamesAreListed(t *testing.T) {
	t.Parallel()

	_, err := ParseDirectives("-- godwit: add-index users (a) nope=1")
	want := "known: name=, unique, using=, where="
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
