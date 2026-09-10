package githubapp

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func repoWith(t *testing.T, touched []string, files map[string]string) *fakeRepo {
	t.Helper()
	repo := writer(t)
	repo.touched, repo.files = touched, files
	repo.submitted = []review{{login: "bob", state: "APPROVED"}}

	return repo
}

const ordersYAML = "dir: db/migrations\ntarget: orders\n"

func plan(t *testing.T, f *fixture) string {
	t.Helper()

	return f.post(t, eventPullRequest, "d1", pullBodyJSON("synchronize", testRepo, testHead)).Body.String()
}

func TestOnlyAChangeTheProjectPlansTriggersIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		touched []string
		want    string
	}{
		{"a migration", []string{"db/migrations/20260101000000_a.up.sql"}, "godwit plan accepted"},
		{"the project file", []string{"godwit.yaml"}, "godwit plan accepted"},
		{"application code", []string{"src/app.js"}, "no bound project"},
		{"a readme beside the migrations", []string{"db/migrations/README.md"}, "no bound project"},
		{"sql that is not a migration", []string{"db/seeds/data.sql"}, "no bound project"},
		{"nothing at all", nil, "no bound project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, bound, repoWith(t, tc.touched, map[string]string{"godwit.yaml": ordersYAML}))
			body := plan(t, f)
			if !strings.Contains(body, tc.want) {
				t.Fatalf("body = %q, want it to carry %q", body, tc.want)
			}
			if strings.Contains(tc.want, "no bound") && len(f.runner.got) != 0 {
				t.Fatal("an untouched project reached the runner")
			}
		})
	}
}

func TestNothingToPlanIsSilenceForAPushAndAReasonForAPerson(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"src/app.js"}, map[string]string{"godwit.yaml": ordersYAML})
	f := newFixture(t, bound, repo)
	rec := f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead))
	if got := f.result(t); got != resultIgnored {
		t.Fatalf("an untouched pull request = %s, want %s", got, resultIgnored)
	}
	check(t, rec, http.StatusAccepted, "no bound project")

	f2 := newFixture(t, bound, repoWith(t, []string{"src/app.js"}, map[string]string{"godwit.yaml": ordersYAML}))
	rec2 := f2.post(t, eventIssueComment, "d2", commentBody("godwit apply", "MEMBER", "alice", now))
	if got := f2.result(t); got != resultRefused {
		t.Fatalf("a command with nothing to do = %s, want %s", got, resultRefused)
	}
	check(t, rec2, http.StatusAccepted, "changes nothing any bound project")
}

func TestWhenModifiedWidensAndNeverNarrows(t *testing.T) {
	t.Parallel()

	yaml := ordersYAML + "autoplan:\n  when_modified: [\"prisma/**\"]\n"
	t.Run("the pattern the project added", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, bound, repoWith(t, []string{"prisma/schema.prisma"}, map[string]string{"godwit.yaml": yaml}))
		if body := plan(t, f); !strings.Contains(body, "godwit plan accepted") {
			t.Fatalf("body = %q", body)
		}
	})
	t.Run("and the migrations it did not repeat", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, bound, repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"},
			map[string]string{"godwit.yaml": yaml}))
		if body := plan(t, f); !strings.Contains(body, "godwit plan accepted") {
			t.Fatalf("body = %q", body)
		}
	})
}

func TestAutoplanDisabledStopsThePushButNotThePerson(t *testing.T) {
	t.Parallel()

	yaml := ordersYAML + "autoplan:\n  enabled: false\n"
	files := map[string]string{"godwit.yaml": yaml}
	touched := []string{"db/migrations/20260101000000_a.up.sql"}

	f := newFixture(t, bound, repoWith(t, touched, files))
	if body := plan(t, f); !strings.Contains(body, "no bound project") {
		t.Fatalf("a disabled project planned on a push: %q", body)
	}

	commanded := newFixture(t, bound, repoWith(t, touched, files))
	check(t, commanded.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, "godwit apply accepted")
}

var monorepo = map[string]string{
	"orders":   testRepo + ":services/orders",
	"billing":  testRepo + ":services/billing",
	"payments": "other/repo",
}

func TestAPullRequestTouchingSeveralProjectsPlansThemAll(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{
		"services/orders/db/migrations/20260101000000_a.up.sql",
		"services/billing/db/migrations/20260101000000_b.up.sql",
		"README.md",
	}, map[string]string{
		"services/orders/godwit.yaml":  ordersYAML,
		"services/billing/godwit.yaml": "dir: db/migrations\ntarget: billing\n",
	})
	f := newFixture(t, monorepo, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "for billing (services/billing), orders (services/orders)")
	if got := f.runner.got[0].projects; len(got) != 2 {
		t.Fatalf("projects = %+v, want both", got)
	}
}

func TestOneProjectOfAMonorepoIsPlannedAlone(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"services/orders/db/migrations/20260101000000_a.up.sql"},
		map[string]string{
			"services/orders/godwit.yaml":  ordersYAML,
			"services/billing/godwit.yaml": "dir: db/migrations\ntarget: billing\n",
		})
	f := newFixture(t, monorepo, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "for orders (services/orders)")
	if len(repo.read) != 1 || repo.read[0] != "services/orders/godwit.yaml" {
		t.Fatalf("read %v, want only the project the pull request touched", repo.read)
	}
}

func TestAProjectAskingForATargetItIsNotBoundTo(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"},
		map[string]string{"godwit.yaml": "dir: db/migrations\ntarget: payments\n"})
	f := newFixture(t, bound, repo)
	rec := f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead))
	check(t, rec, http.StatusAccepted, `is not bound to a target named "payments"`)
	if len(f.runner.got) != 0 {
		t.Fatal("an unbound target reached the runner")
	}
}

func TestProjectFilesThatCannotBeUsed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, yaml, want string }{
		{"no target", "dir: db/migrations\n", "names no target"},
		{"unparsable", "dir: [\n", "does not parse"},
		{"an unknown key", "dir: db/migrations\ntarget: orders\nnope: 1\n", "does not parse"},
		{"a dir that leaves the project", "dir: ../../etc\ntarget: orders\n", "leaves the project"},
		{"an absolute dir", "dir: /etc\ntarget: orders\n", "is absolute"},
		{
			"a pattern that leaves the project", ordersYAML + "autoplan:\n  when_modified: [\"../../**\"]\n",
			"leaves the project directory",
		},
		{
			"an absolute pattern", ordersYAML + "autoplan:\n  when_modified: [\"/etc/**\"]\n",
			"is absolute",
		},
		{"an empty pattern", ordersYAML + "autoplan:\n  when_modified: [\"\"]\n", "empty pattern"},
		{"a malformed pattern", ordersYAML + "autoplan:\n  when_modified: [\"db/[\"]\n", "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, bound, repoWith(t, []string{"db/migrations/20260101000000_a.up.sql", "godwit.yaml"},
				map[string]string{"godwit.yaml": tc.yaml}))
			rec := f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead))
			check(t, rec, http.StatusAccepted, tc.want)
			if got := f.result(t); got != resultRefused {
				t.Fatalf("result = %s, want %s", got, resultRefused)
			}
		})
	}
}

func TestABoundRootWithNoProjectFile(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, nil))
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "carries no godwit.yaml")
}

func TestTheProjectFileCannotBeRead(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, nil)
	repo.fileErr = errBroken
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "godwit.yaml: broken")
}

func TestTheChangedFilesCannotBeRead(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, nil, nil)
	repo.changedErr = errBroken
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusInternalServerError, "could not answer")
}

func TestGlobMatching(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"db/migrations/**/*.sql", "db/migrations/a.up.sql", true},
		{"db/migrations/**/*.sql", "db/migrations/2026/a.up.sql", true},
		{"db/migrations/**/*.sql", "db/migrations/a.md", false},
		{"db/migrations/**/*.sql", "db/seeds/a.sql", false},
		{"**", "anything/at/all", true},
		{"**/*.sql", "a.sql", true},
		{"*.sql", "db/a.sql", false},
		{"godwit.yaml", "godwit.yaml", true},
		{"godwit.yaml", "db/godwit.yaml", false},
		{"prisma/**", "prisma/schema.prisma", true},
		{"prisma/**", "prisma", false},
		{"a/**/b/*.sql", "a/x/y/b/c.sql", true},
		{"a/**/b/*.sql", "a/x/y/c.sql", false},
		{"?.sql", "a.sql", true},
		{"?.sql", "ab.sql", false},
		{"a/b", "a", false},
	} {
		if got := matchGlob(tc.pattern, tc.name); got != tc.want {
			t.Fatalf("matchGlob(%q, %q) = %t, want %t", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestARootBindingReadsTheRepositoryRoot(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, nil)
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "the repository root carries no godwit.yaml")
	if len(repo.read) != 1 || repo.read[0] != "godwit.yaml" {
		t.Fatalf("read %v", repo.read)
	}
}

func TestAProjectOutsideTheRepositoryRootIsNamedByItsRoot(t *testing.T) {
	t.Parallel()

	p := project{root: "services/orders", target: "orders"}
	if p.String() != "orders (services/orders)" {
		t.Fatalf("String = %q", p.String())
	}
}

func TestAMonorepoRootWithNoProjectFile(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"services/orders/db/migrations/a.up.sql"}, nil)
	f := newFixture(t, monorepo, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "services/orders carries no godwit.yaml")
}

func TestAnEmptyDirIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, repoWith(t, []string{"godwit.yaml"},
		map[string]string{"godwit.yaml": "dir: \"\"\ntarget: orders\n"}))
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "dir is empty")
}

func TestAPlanCannotMintItsInstallationToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	f.api.err = errBroken
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusInternalServerError, "could not answer")
}

func TestATruncatedListingNeverConcludesNothingToPlan(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, event, body string }{
		{"a push", eventPullRequest, pullBodyJSON("synchronize", testRepo, testHead)},
		{"a command", eventIssueComment, commentBody("godwit apply", "MEMBER", "alice", now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := repoWith(t, []string{"src/app.js"}, map[string]string{"godwit.yaml": ordersYAML})
			repo.filesCapped = true
			f := newFixture(t, bound, repo)
			rec := f.post(t, tc.event, "d1", tc.body)
			check(t, rec, http.StatusAccepted, "will not guess")
			if got := f.result(t); got != resultRefused {
				t.Fatalf("result = %s, want %s", got, resultRefused)
			}
			if strings.Contains(rec.Body.String(), "no bound project") {
				t.Fatalf("a partial listing was read as nothing to do: %q", rec.Body.String())
			}
			if len(f.runner.got) != 0 {
				t.Fatal("a partial listing enqueued work")
			}
		})
	}
}

func TestATruncatedListingIsRefusedEvenWhenSomethingMatched(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	repo.filesCapped = true
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "will not guess")
	if len(f.runner.got) != 0 {
		t.Fatal("a project matched from a partial listing was planned; the projects it did not see were not")
	}
}

func TestAListingShorterThanThePullRequestSaysIsTruncated(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"src/app.js"}, map[string]string{"godwit.yaml": ordersYAML})
	f := newFixture(t, bound, repo)
	body := strings.Replace(pullBodyJSON("opened", testRepo, testHead), `"number":3`, `"number":3,"changed_files":9000`, 1)
	rec := f.post(t, eventPullRequest, "d1", body)
	check(t, rec, http.StatusAccepted, "listed 1 of the 9000 files")
	if got := f.result(t); got != resultRefused {
		t.Fatalf("result = %s, want %s", got, resultRefused)
	}
}

func TestAWholeListingIsBelievedWhateverTheCountSays(t *testing.T) {
	t.Parallel()

	for _, files := range []int{0, 1} {
		f := newFixture(t, bound, repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"},
			map[string]string{"godwit.yaml": ordersYAML}))
		body := strings.Replace(pullBodyJSON("opened", testRepo, testHead),
			`"number":3`, fmt.Sprintf(`"number":3,"changed_files":%d`, files), 1)
		check(t, f.post(t, eventPullRequest, "d1", body), http.StatusAccepted, "godwit plan accepted")
	}
}

func TestTruncationIsCaughtFromTheApiCountOnACommand(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	repo.pr.files = 4000
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, "listed 1 of the 4000 files")
}

func TestATruncatedReviewListingRefusesTheApply(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	repo.reviewsCapped = true
	f := newFixture(t, bound, repo)
	rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	check(t, rec, http.StatusAccepted, "the ones that withdraw an approval are the ones it would miss")
	if len(f.runner.got) != 0 {
		t.Fatal("an apply was decided on a part of the review record")
	}
}
