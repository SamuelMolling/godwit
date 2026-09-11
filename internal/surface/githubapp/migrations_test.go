package githubapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/limits"
)

func listed(names ...string) contents {
	out := contents{}
	for _, n := range names {
		out.entries = append(out.entries, content{name: n, size: len(n), file: true})
	}

	return out
}

const testDir = "db/migrations"

func withBodies(names ...string) *fakeRepo {
	r := &fakeRepo{listing: map[string]contents{testDir: listed(names...)}, blobs: map[string]string{}}
	for _, n := range names {
		r.blobs[testDir+"/"+n] = "-- " + n
	}

	return r
}

func names(files []*godwitv1.MigrationFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}

	return out
}

func TestTheMigrationSetIsTheDirectoryAtTheHead(t *testing.T) {
	t.Parallel()

	repo := withBodies("20260102000000_b.up.sql", "20260101000000_a.up.sql", "20260101000000_a.down.sql")
	got, err := migrations(context.Background(), repo, "db/migrations", testHead, limits.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"20260101000000_a.down.sql", "20260101000000_a.up.sql", "20260102000000_b.up.sql"}
	if fmt.Sprint(names(got)) != fmt.Sprint(want) {
		t.Fatalf("files = %v", names(got))
	}
	if got[0].Body != "-- 20260101000000_a.down.sql" {
		t.Fatalf("body = %q", got[0].Body)
	}
}

func TestAPartialDirectoryIsRefusedRatherThanPlanned(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{listing: map[string]contents{"db/migrations": {entries: listed("a.up.sql").entries, capped: true}}}
	_, err := migrations(context.Background(), repo, "db/migrations", testHead, limits.Limits{})
	if !errors.Is(err, errPartial) || !strings.Contains(err.Error(), "contents api") {
		t.Fatalf("err = %v", err)
	}
	if len(repo.fetched) != 0 {
		t.Fatalf("fetched %v from a listing it could not trust", repo.fetched)
	}
}

func TestTheLimitsAreAppliedToTheListingBeforeAnyBodyIsFetched(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		lim  limits.Limits
		want string
	}{
		{"a file over max-file-bytes", limits.Limits{FileBytes: 4}, "20260101000000_a.up.sql is 23 bytes, limit 4"},
		{"more files than max-files", limits.Limits{Files: 1}, "too many migration files: 2, limit 1"},
		{"more migrations than max-migrations", limits.Limits{Migrations: 1}, "too many migrations: 2, limit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := withBodies("20260101000000_a.up.sql", "20260102000000_b.up.sql")
			_, err := migrations(context.Background(), repo, "db/migrations", testHead, tc.lim)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
			if len(repo.fetched) != 0 {
				t.Fatalf("fetched %v before the listing was admitted", repo.fetched)
			}
		})
	}
}

func TestADirectoryThatIsNotThereYetIsAnEmptySet(t *testing.T) {
	t.Parallel()

	got, err := migrations(context.Background(), &fakeRepo{}, "db/migrations", testHead, limits.Limits{})
	if err != nil || got != nil {
		t.Fatalf("files = %v, err = %v", got, err)
	}
}

func TestADirectoryHoldingNothingGodwitReadsIsAnEmptySet(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{listing: map[string]contents{"db/migrations": {entries: []content{
		{name: ".keep", file: true}, {name: "archive", file: false},
	}}}}
	got, err := migrations(context.Background(), repo, "db/migrations", testHead, limits.Limits{})
	if err != nil || got != nil {
		t.Fatalf("files = %v, err = %v", got, err)
	}
	if len(repo.fetched) != 0 {
		t.Fatalf("fetched %v", repo.fetched)
	}
}

func TestADirectoryGodwitCannotList(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{listErr: errBroken}
	if _, err := migrations(context.Background(), repo, "db/migrations", testHead, limits.Limits{}); !errors.Is(err, errBroken) {
		t.Fatalf("err = %v", err)
	}
}

func TestABodyGodwitCannotReadNamesTheFile(t *testing.T) {
	t.Parallel()

	repo := withBodies("20260101000000_a.up.sql")
	delete(repo.blobs, "db/migrations/20260101000000_a.up.sql")
	_, err := migrations(context.Background(), repo, "db/migrations", testHead, limits.Limits{})
	if err == nil || !strings.Contains(err.Error(), "db/migrations/20260101000000_a.up.sql") {
		t.Fatalf("err = %v", err)
	}
}

func TestOneUnreadableBodyStopsTheWholeSet(t *testing.T) {
	t.Parallel()

	all := make([]string, 0, 3*blobWorkers)
	for i := range cap(all) {
		all = append(all, fmt.Sprintf("%014d_m.up.sql", i))
	}
	repo := withBodies(all...)
	repo.blobErr = errBroken
	if _, err := migrations(context.Background(), repo, "db/migrations", testHead, limits.Limits{}); !errors.Is(err, errBroken) {
		t.Fatalf("err = %v", err)
	}
	if len(repo.fetched) == len(all) {
		t.Fatalf("fetched every one of %d files after the first failed", len(all))
	}
}
