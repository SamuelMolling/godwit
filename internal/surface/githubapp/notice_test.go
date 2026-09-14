package githubapp

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGodwitAcknowledgesTheCommentItRead(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	f := newFixture(t, bound, repo)
	body := strings.Replace(commentBody("godwit apply", "MEMBER", "alice", now), `"body":"godwit apply"`,
		`"id":77,"body":"godwit apply"`, 1)
	check(t, f.post(t, eventIssueComment, "d1", body), http.StatusAccepted, "godwit apply accepted")
	if len(repo.reacted) != 1 || repo.reacted[0] != "77:eyes" {
		t.Fatalf("reacted = %v, want the commanding comment marked once", repo.reacted)
	}
}

func TestTheAcknowledgementComesBeforeTheAnswer(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	repo.perm = map[string]string{}
	f := newFixture(t, bound, repo)
	body := strings.Replace(commentBody("godwit apply", "MEMBER", "alice", now), `"body":"godwit apply"`,
		`"id":77,"body":"godwit apply"`, 1)
	check(t, f.post(t, eventIssueComment, "d1", body), http.StatusAccepted, "not write or admin")
	if len(repo.reacted) != 1 {
		t.Fatalf("reacted = %v, want a refused command still acknowledged as read", repo.reacted)
	}
	if len(repo.notices) != 1 || !strings.Contains(repo.notices[0], "not write or admin") {
		t.Fatalf("notices = %v", repo.notices)
	}
}

func TestNoReactionIsConfigurable(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	f := newFixture(t, bound, repo)
	f.receiver.cfg.Reaction = noReaction
	body := strings.Replace(commentBody("godwit apply", "MEMBER", "alice", now), `"body":"godwit apply"`,
		`"id":77,"body":"godwit apply"`, 1)
	check(t, f.post(t, eventIssueComment, "d1", body), http.StatusAccepted, "accepted")
	if len(repo.reacted) != 0 {
		t.Fatalf("reacted = %v, want none", repo.reacted)
	}
}

func TestAPushHasNoCommentToAcknowledge(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "accepted")
	if len(repo.reacted) != 0 {
		t.Fatalf("reacted = %v", repo.reacted)
	}
}

func TestAFailedAcknowledgementNeverFailsTheDelivery(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": ordersYAML})
	repo.reactErr = errBroken
	f := newFixture(t, bound, repo)
	body := strings.Replace(commentBody("godwit apply", "MEMBER", "alice", now), `"body":"godwit apply"`,
		`"id":77,"body":"godwit apply"`, 1)
	check(t, f.post(t, eventIssueComment, "d1", body), http.StatusAccepted, "godwit apply accepted")
}

func TestARefusalIsSaidWhereTheAuthorIsLooking(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, yaml, want string }{
		{"a project file that does not parse", "dir: [\n", "does not parse"},
		{"a target the repository is not bound to", "dir: db/migrations\ntarget: payments\n", "not bound to a target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"},
				map[string]string{"godwit.yaml": tc.yaml})
			f := newFixture(t, bound, repo)
			f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead))
			if len(repo.notices) != 1 || !strings.Contains(repo.notices[0], tc.want) {
				t.Fatalf("notices = %v, want one carrying %q", repo.notices, tc.want)
			}
			if !strings.HasPrefix(repo.notices[0], refusedMarker+"\n## godwit plan refused\n") ||
				!strings.HasSuffix(repo.notices[0], "\n\nNothing ran.\n") {
				t.Fatalf("notice = %q", repo.notices[0])
			}
			if len(repo.checks) != 1 || !strings.HasPrefix(repo.checks[0], "godwit/plan@"+testHead) {
				t.Fatalf("checks = %v, want the check a reviewer already knows turned red", repo.checks)
			}
		})
	}
}

func TestSilenceIsNeverACommentAndNorIsSuccess(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, event, body string }{
		{"a pull request that changed nothing godwit plans", eventPullRequest, pullBodyJSON("opened", testRepo, testHead)},
		{"a comment that names no command", eventIssueComment, commentBody("looks good", "MEMBER", "alice", now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := repoWith(t, []string{"src/app.js"}, map[string]string{"godwit.yaml": ordersYAML})
			f := newFixture(t, bound, repo)
			f.post(t, tc.event, "d1", tc.body)
			if len(repo.notices) != 0 {
				t.Fatalf("notices = %v, want silence", repo.notices)
			}
		})
	}
}

func TestAFailedNoticeNeverFailsTheDelivery(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": "dir: [\n"})
	repo.noticeErr = errBroken
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "does not parse")
}

func TestANoticeNeedsAPullRequestToSayItOn(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	check(t, f.post(t, eventIssueComment, "d1", `{"repository":{"full_name":"acme/orders"}}`),
		http.StatusAccepted, "names no installation")
	if len(f.api.scoped) != 0 {
		t.Fatal("a payload with no pull request still opened a repository view")
	}
}

func TestANoticeThatCannotOpenTheRepositoryIsLoggedAndDropped(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": "dir: [\n"})
	f := newFixture(t, bound, repo)
	f.api.err = errBroken
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusInternalServerError, "could not answer")
}

func TestARefusalGodwitNeverGotPastAuthorizationIsTheCommentAlone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, want string
		at         time.Time
		mutate     func(repo *fakeRepo)
	}{
		{"a delivery too old to act on", "past the 1h0m0s", now.Add(-2 * time.Hour), func(*fakeRepo) {}},
		{
			"a commander godwit would not let command it", "not write or admin", now,
			func(repo *fakeRepo) { repo.perm = map[string]string{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"},
				map[string]string{"godwit.yaml": ordersYAML})
			tc.mutate(repo)
			f := newFixture(t, bound, repo)
			check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", tc.at)),
				http.StatusAccepted, tc.want)
			if len(repo.notices) != 1 {
				t.Fatalf("notices = %v, want the commander still told why", repo.notices)
			}
			if len(repo.checks) != 0 {
				t.Fatalf("checks = %v, want none: a check a commander is not allowed to ask for is one they could "+
					"turn red on a head godwit already passed", repo.checks)
			}
		})
	}
}

func TestARefusalPastAuthorizationMarksTheHeadGodwitJudged(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"src/app.js"}, map[string]string{"godwit.yaml": ordersYAML})
	repo.pr.Head = testOther
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, "changes nothing any bound project")
	if len(repo.checks) != 1 || !strings.HasPrefix(repo.checks[0], "godwit/applied@"+testOther) {
		t.Fatalf("checks = %v, want one on the head authorization resolved", repo.checks)
	}
}

func TestARefusedCheckThatFailsNeverFailsTheDelivery(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, []string{"db/migrations/20260101000000_a.up.sql"}, map[string]string{"godwit.yaml": "dir: [\n"})
	repo.checkErr = errBroken
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead)),
		http.StatusAccepted, "does not parse")
}

func TestAnUnboundRepositoryIsAnsweredOnNoEventAtAll(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, nil, nil)
	f := newFixture(t, map[string]string{"payments": "someone/else"}, repo)
	f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", testRepo, testHead))
	if len(repo.notices) != 0 || len(repo.checks) != 0 {
		t.Fatalf("notices = %v, checks = %v, want silence", repo.notices, repo.checks)
	}
}

func TestAMalformedCommandIsAnsweredWithoutACheckItNeverNamed(t *testing.T) {
	t.Parallel()

	repo := repoWith(t, nil, nil)
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply --unknown", "MEMBER", "alice", now)),
		http.StatusAccepted, "does not understand '--unknown'")
	if len(repo.notices) != 1 {
		t.Fatalf("notices = %v", repo.notices)
	}
	if len(repo.checks) != 0 {
		t.Fatalf("checks = %v, want none: the comment named no command to have a check", repo.checks)
	}
}

func TestARefusalGodwitCannotEvenOpenTheRepositoryToSay(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, repoWith(t, nil, nil))
	f.api.err = errBroken
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply --unknown", "MEMBER", "alice", now)),
		http.StatusAccepted, "does not understand '--unknown'")
}
