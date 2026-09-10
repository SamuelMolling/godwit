package githubapp

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/api"
)

// errPartial is a migration directory godwit knows it did not read whole, which it refuses rather than plans.
var errPartial = errors.New("partial")

// blobWorkers is how many bodies one command fetches at once; GitHub answers each on its own request.
const blobWorkers = 8

// migrations reads a directory at head; the limits are decided from the listing, so one over them
// costs a single request rather than one per file.
func migrations(ctx context.Context, repo repoView, dir, head string, lim api.Limits) ([]*godwitv1.MigrationFile, error) {
	lim = lim.WithDefaults()
	listed, err := repo.directory(ctx, dir, head)
	if errors.Is(err, errAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if listed.capped {
		return nil, fmt.Errorf("%w: github answered %d entries for %s at %s, which is every entry its contents api "+
			"lists for one directory", errPartial, len(listed.entries), dir, short(head))
	}
	want := wanted(listed)
	if err := lim.CheckListing(want); err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return nil, nil
	}

	return bodies(ctx, repo, dir, head, want, lim.FileBytes)
}

// wanted is what engine.LoadFS would read out of the same directory on disk: files, dot-files left out.
func wanted(listed contents) []api.Listed {
	out := make([]api.Listed, 0, len(listed.entries))
	for _, e := range listed.entries {
		if !e.file || strings.HasPrefix(e.name, ".") {
			continue
		}
		out = append(out, api.Listed{Name: e.name, Size: e.size})
	}
	slices.SortFunc(out, func(a, b api.Listed) int { return strings.Compare(a.Name, b.Name) })

	return out
}

func bodies(ctx context.Context, repo repoView, dir, head string, want []api.Listed, limit int) ([]*godwitv1.MigrationFile, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]*godwitv1.MigrationFile, len(want))
	next, errs := make(chan int), make(chan error, blobWorkers)
	var wg sync.WaitGroup
	for range min(blobWorkers, len(want)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				body, err := repo.blob(ctx, path.Join(dir, want[i].Name), head, limit)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", path.Join(dir, want[i].Name), err)
					cancel()

					return
				}
				out[i] = &godwitv1.MigrationFile{Name: want[i].Name, Body: string(body)}
			}
		}()
	}
	feed(ctx, next, len(want))
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}

	return out, nil
}

func feed(ctx context.Context, next chan<- int, n int) {
	defer close(next)
	for i := range n {
		select {
		case next <- i:
		case <-ctx.Done():
			return
		}
	}
}
