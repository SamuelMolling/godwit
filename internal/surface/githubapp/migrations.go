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
	"github.com/SamuelMolling/godwit/internal/limits"
)

var errPartial = errors.New("partial")

const blobWorkers = 8

func migrations(ctx context.Context, repo repoView, dir, head string, lim limits.Limits) ([]*godwitv1.MigrationFile, error) {
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

func wanted(listed contents) []limits.Listed {
	out := make([]limits.Listed, 0, len(listed.entries))
	for _, e := range listed.entries {
		if !e.file || strings.HasPrefix(e.name, ".") {
			continue
		}
		out = append(out, limits.Listed{Name: e.name, Size: e.size})
	}
	slices.SortFunc(out, func(a, b limits.Listed) int { return strings.Compare(a.Name, b.Name) })

	return out
}

func bodies(ctx context.Context, repo repoView, dir, head string, want []limits.Listed, limit int) ([]*godwitv1.MigrationFile, error) {
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
