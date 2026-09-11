package limits

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Parallel()

	l := Limits{}.WithDefaults()
	if l.RequestBytes != DefaultRequestBytes || l.Migrations != DefaultMigrations || l.Files != DefaultFiles ||
		l.FileBytes != DefaultFileBytes || l.HeavyCalls != DefaultHeavyCalls || l.HeavyWait != DefaultHeavyWait {
		t.Fatalf("defaults = %+v", l)
	}
	if DefaultFiles < 2*DefaultMigrations {
		t.Fatalf("the file limit must clear two files per migration at the migration limit: %d < %d",
			DefaultFiles, 2*DefaultMigrations)
	}
	set := Limits{RequestBytes: 1, Migrations: 9, Files: 2, FileBytes: 3, HeavyCalls: 4, HeavyWait: time.Second}
	if got := set.WithDefaults(); got != set {
		t.Fatalf("explicit limits = %+v, want %+v", got, set)
	}
}

func directory(n, size int) []Listed {
	out := make([]Listed, 0, 2*n)
	for i := range n {
		id := "2026090112" + strconv.Itoa(i) + "_t"
		out = append(out, Listed{Name: id + ".up.sql", Size: size}, Listed{Name: id + ".down.sql", Size: size})
	}

	return out
}

func TestCheckListing(t *testing.T) {
	t.Parallel()

	l := Limits{}.WithDefaults()
	if err := l.CheckListing(directory(200, 7800)); err != nil {
		t.Fatalf("a 200-migration directory must be admitted: %v", err)
	}
	deep := append(directory(1000, 10), Listed{Name: "20260902000000_squash.up.sql", Size: 22})
	if err := l.CheckListing(deep); err != nil {
		t.Fatalf("a 1000-migration directory and its checkpoint must be admitted: %v", err)
	}

	cases := []struct {
		name string
		in   []Listed
		want string
	}{
		{"too many files", make([]Listed, l.Files+1), "too many migration files"},
		{"too many migrations", directory(l.Migrations+1, 0), "too many migrations"},
		{"long name", []Listed{{Name: strings.Repeat("n", maxNameBytes+1)}}, "file name is"},
		{"big body", []Listed{{Name: "a.up.sql", Size: l.FileBytes + 1}}, "bytes, limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := l.CheckListing(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
