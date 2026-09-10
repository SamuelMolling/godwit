package githubapp

import (
	"context"
	"sync"
	"time"

	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// fakeRuns is the binding table: an in-memory version of the claim the store makes atomic.
type fakeRuns struct {
	mu       sync.Mutex
	rows     []controlplane.GitHubRun
	reported map[string]string
	claimed  map[string]bool
	recorded []controlplane.GitHubRun
	recErr   error
	claimErr error
	markErr  error
	listErr  error
}

func newRunStore() *fakeRuns {
	return &fakeRuns{reported: map[string]string{}, claimed: map[string]bool{}}
}

func (f *fakeRuns) RecordGitHubRun(_ context.Context, g controlplane.GitHubRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recErr != nil {
		return f.recErr
	}
	f.recorded = append(f.recorded, g)
	for i, row := range f.rows {
		if row.RunID == g.RunID {
			g.State, g.Reverts = row.State, row.Reverts
			f.rows[i] = g
			delete(f.reported, g.RunID)

			return nil
		}
	}
	f.rows = append(f.rows, g)

	return nil
}

func (f *fakeRuns) ClaimGitHubReports(_ context.Context, _ time.Duration, limit int) ([]controlplane.GitHubRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	var out []controlplane.GitHubRun
	for _, g := range f.rows {
		if len(out) == limit {
			break
		}
		if !reportableState(g.State) || f.reported[g.RunID] == g.State || f.claimed[g.RunID] {
			continue
		}
		f.claimed[g.RunID] = true
		out = append(out, g)
	}

	return out, nil
}

func (f *fakeRuns) MarkGitHubReported(_ context.Context, runID, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.reported[runID] = state
	delete(f.claimed, runID)

	return nil
}

func (f *fakeRuns) GitHubRunsOf(_ context.Context, repository string, pull int, target string) ([]controlplane.GitHubRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []controlplane.GitHubRun
	for i := len(f.rows) - 1; i >= 0; i-- {
		g := f.rows[i]
		if g.Repository == repository && g.PullRequest == pull && g.Target == target {
			out = append(out, g)
		}
	}

	return out, nil
}

func (f *fakeRuns) put(g controlplane.GitHubRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, g)
}

func (f *fakeRuns) setState(runID, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, g := range f.rows {
		if g.RunID == runID {
			f.rows[i].State = state
		}
	}
}

func (f *fakeRuns) told() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reported["run-7"]
}

func (f *fakeRuns) bound() []controlplane.GitHubRun {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]controlplane.GitHubRun(nil), f.recorded...)
}

func reportableState(state string) bool {
	switch state {
	case controlplane.StateSucceeded, controlplane.StateFailed,
		controlplane.StateNeedsAttention, controlplane.StateAwaitingContract:
		return true
	}

	return false
}
