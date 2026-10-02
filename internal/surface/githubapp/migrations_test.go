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

func sized(n, size int, body string) *fakeRepo {
	r := &fakeRepo{blobs: map[string]string{}}
	var listed contents
	for i := range n {
		name := fmt.Sprintf("%014d_m.up.sql", i)
		listed.entries = append(listed.entries, content{name: name, size: size, file: true})
		r.blobs[testDir+"/"+name] = body
	}
	r.listing = map[string]contents{testDir: listed}

	return r
}

func TestADirectoryOverTheAggregateBoundIsRefusedBeforeAnyBodyIsFetched(t *testing.T) {
	t.Parallel()

	lim := limits.Limits{RequestBytes: 8 << 20, FileBytes: 1 << 20}
	repo := sized(100, 1<<20, strings.Repeat("x", 1<<20))
	_, err := migrations(context.Background(), repo, testDir, testHead, lim)
	if err == nil || !strings.Contains(err.Error(), "over the 8388608 bytes a request may hold in total") {
		t.Fatalf("err = %v", err)
	}
	if len(repo.fetched) != 0 {
		t.Fatalf("fetched %d bodies of a set the listing already put over the bound", len(repo.fetched))
	}
}

func TestAListingThatUnderstatesItsSizesIsRefusedPartWayThroughTheFetch(t *testing.T) {
	t.Parallel()

	const body = 1 << 20
	lim := limits.Limits{RequestBytes: 8 << 20, FileBytes: 1 << 20}
	repo := sized(100, 0, strings.Repeat("x", body))
	_, err := migrations(context.Background(), repo, testDir, testHead, lim)
	if err == nil || !strings.Contains(err.Error(), "over the 8388608 bytes a request may hold in total") ||
		!strings.Contains(err.Error(), testDir+"/") {
		t.Fatalf("err = %v", err)
	}
	held := len(repo.fetched) * body
	if ceiling := lim.RequestBytes + (blobWorkers+1)*lim.FileBytes; held > ceiling {
		t.Fatalf("held %d bytes before refusing, over the %d the bound and the fetches in flight allow", held, ceiling)
	}
}
