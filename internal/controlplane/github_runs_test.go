package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"
)

const (
	runOne = "11111111-1111-1111-1111-111111111111"
	runTwo = "22222222-2222-2222-2222-222222222222"
)

func withRun(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateRun(ctx, id, "orders", RolloutDirect, goodFiles(), Timeouts{}, Provenance{}, "", nil); err != nil {
		t.Fatal(err)
	}
}

func githubRun(id string) GitHubRun {
	return GitHubRun{
		RunID: id, Repository: "acme/orders", RepositoryID: 42, Installation: 7, PullRequest: 3,
		Head: "1111111111111111111111111111111111111111", Command: "apply", Target: "orders",
		Marker: "<!-- godwit:migrate -->", Format: "schema", CheckRun: 91,
	}
}

func storeWithTarget(t *testing.T) *Store {
	t.Helper()
	s, _ := newStore(t)
	if err := s.RegisterTarget(context.Background(), "orders", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}

	return s
}

func TestAGitHubRunIsClaimedOnceForEachStateItReaches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storeWithTarget(t)
	withRun(t, s, runOne)
	if err := s.RecordGitHubRun(ctx, githubRun(runOne)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ClaimGitHubReports(ctx, time.Minute, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("claimed %d before the run settled, %v", len(got), err)
	}
	if err := s.Finish(ctx, runOne, StateAwaitingContract, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.ClaimGitHubReports(ctx, time.Minute, 10)
	if err != nil || len(got) != 1 || got[0].State != StateAwaitingContract {
		t.Fatalf("claimed = %+v, %v", got, err)
	}
	if got[0].CheckRun != 91 || got[0].Marker != "<!-- godwit:migrate -->" || got[0].PullRequest != 3 {
		t.Fatalf("binding lost its context: %+v", got[0])
	}
	if err := s.MarkGitHubReported(ctx, runOne, StateAwaitingContract); err != nil {
		t.Fatal(err)
	}
	if got, err = s.ClaimGitHubReports(ctx, time.Minute, 10); err != nil || len(got) != 0 {
		t.Fatalf("the same state was claimed twice: %+v, %v", got, err)
	}

	if err := s.Finish(ctx, runOne, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if got, err = s.ClaimGitHubReports(ctx, time.Minute, 10); err != nil || len(got) != 1 {
		t.Fatalf("the contract phase was not claimed: %+v, %v", got, err)
	}
}

func TestAClaimIsHeldForItsLeaseAndNoLonger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storeWithTarget(t)
	withRun(t, s, runOne)
	if err := s.RecordGitHubRun(ctx, githubRun(runOne)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, runOne, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimGitHubReports(ctx, time.Minute, 10); err != nil || len(got) != 1 {
		t.Fatalf("first claim = %+v, %v", got, err)
	}
	if got, err := s.ClaimGitHubReports(ctx, time.Minute, 10); err != nil || len(got) != 0 {
		t.Fatalf("a live claim was taken again: %+v, %v", got, err)
	}
	if got, err := s.ClaimGitHubReports(ctx, 0, 10); err != nil || len(got) != 1 {
		t.Fatalf("a lapsed claim was not retaken: %+v, %v", got, err)
	}
}

func TestConcurrentClaimantsGetTheRunOnlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storeWithTarget(t)
	withRun(t, s, runOne)
	if err := s.RecordGitHubRun(ctx, githubRun(runOne)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, runOne, StateSucceeded, ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	claims := make([]int, 8)
	for i := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimGitHubReports(ctx, time.Minute, 10)
			if err == nil {
				claims[i] = len(got)
			}
		}()
	}
	wg.Wait()
	total := 0
	for _, n := range claims {
		total += n
	}
	if total != 1 {
		t.Fatalf("the run was claimed %d times", total)
	}
}

func TestABindingIsReplacedWhenASecondCommandTakesTheRunOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storeWithTarget(t)
	withRun(t, s, runOne)
	if err := s.RecordGitHubRun(ctx, githubRun(runOne)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, runOne, StateAwaitingContract, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimGitHubReports(ctx, time.Minute, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkGitHubReported(ctx, runOne, StateAwaitingContract); err != nil {
		t.Fatal(err)
	}

	taken := githubRun(runOne)
	taken.Command, taken.CheckRun = "confirm", 92
	if err := s.RecordGitHubRun(ctx, taken); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimGitHubReports(ctx, time.Minute, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("claimed = %+v, %v", got, err)
	}
	if got[0].CheckRun != 92 || got[0].Command != "confirm" {
		t.Fatalf("binding = %+v", got[0])
	}
}

func TestGitHubRunsOfOnePullRequestAreNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storeWithTarget(t)
	withRun(t, s, runOne)
	withRun(t, s, runTwo)
	for _, id := range []string{runOne, runTwo} {
		if err := s.RecordGitHubRun(ctx, githubRun(id)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GitHubRunsOf(ctx, "acme/orders", 3, "orders")
	if err != nil || len(got) != 2 {
		t.Fatalf("runs = %+v, %v", got, err)
	}
	if got[0].RunID != runTwo || got[1].RunID != runOne {
		t.Fatalf("order = %s, %s", got[0].RunID, got[1].RunID)
	}
	if none, err := s.GitHubRunsOf(ctx, "acme/orders", 4, "orders"); err != nil || len(none) != 0 {
		t.Fatalf("another pull request = %+v, %v", none, err)
	}
	if none, err := s.GitHubRunsOf(ctx, "acme/orders", 3, "billing"); err != nil || len(none) != 0 {
		t.Fatalf("another target = %+v, %v", none, err)
	}
}

func TestABindingIsSweptOnlyOnceItIsReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storeWithTarget(t)
	withRun(t, s, runOne)
	if err := s.RecordGitHubRun(ctx, githubRun(runOne)); err != nil {
		t.Fatal(err)
	}
	n, err := s.SweepGitHubRuns(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("swept an unreported binding: %d, %v", n, err)
	}
	if err := s.MarkGitHubReported(ctx, runOne, StateSucceeded); err != nil {
		t.Fatal(err)
	}
	if n, err = s.SweepGitHubRuns(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("early sweep = %d, %v", n, err)
	}
	if n, err = s.SweepGitHubRuns(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
}

func TestAGitHubRunRowThatWillNotScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storeWithTarget(t)
	withRun(t, s, runOne)
	if err := s.RecordGitHubRun(ctx, githubRun(runOne)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`ALTER TABLE cp_github_runs ALTER COLUMN pull_request TYPE text USING pull_request::text`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GitHubRunsOf(ctx, "acme/orders", 3, "orders"); err == nil {
		t.Fatal("no error")
	}
}

func TestGitHubRunStoreFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)

	if _, err := pool.Exec(ctx, "DROP TABLE cp_github_runs"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGitHubRun(ctx, githubRun(runOne)); err == nil {
		t.Fatal("no error from RecordGitHubRun")
	}
	if _, err := s.ClaimGitHubReports(ctx, time.Minute, 1); err == nil {
		t.Fatal("no error from ClaimGitHubReports")
	}
	if err := s.MarkGitHubReported(ctx, runOne, StateSucceeded); err == nil {
		t.Fatal("no error from MarkGitHubReported")
	}
	if _, err := s.GitHubRunsOf(ctx, "acme/orders", 3, "orders"); err == nil {
		t.Fatal("no error from GitHubRunsOf")
	}
	if _, err := s.SweepGitHubRuns(ctx, time.Now()); err == nil {
		t.Fatal("no error from SweepGitHubRuns")
	}
}
