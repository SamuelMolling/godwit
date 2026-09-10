package githubapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var bound = map[string]string{"orders": testRepo, "billing": "other/repo"}

func check(t *testing.T, rec *httptest.ResponseRecorder, status int, want string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, status, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body = %q, want it to carry %q", rec.Body.String(), want)
	}
}

func (f *fixture) result(t *testing.T) string {
	t.Helper()
	if len(f.counts) != 1 {
		t.Fatalf("counted %v, want exactly one delivery", f.counts)
	}

	return f.counts[0].result
}

func TestUnverifiedRequestLearnsNothing(t *testing.T) {
	t.Parallel()

	body := commentBody("godwit apply", "MEMBER", "alice", now)
	for _, tc := range []struct{ name, signature string }{
		{"missing", ""},
		{"wrong secret", sign("other", body)},
		{"wrong body", sign(testSecret, "{}")},
		{"no prefix", strings.TrimPrefix(sign(testSecret, body), signaturePrefix)},
		{"sha1 is never a fallback", "sha1=" + strings.TrimPrefix(sign(testSecret, body), signaturePrefix)},
		{"empty hex", signaturePrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, bound, writer(t))
			rec := f.send(t, EventIssueComment, "d1", body, tc.signature)
			check(t, rec, http.StatusUnauthorized, "")
			if rec.Body.Len() != 0 {
				t.Fatalf("body = %q, want nothing", rec.Body.String())
			}
			if f.store.commits != 0 || len(f.api.scoped) != 0 {
				t.Fatalf("an unverified delivery touched the store or github")
			}
			if got := f.result(t); got != resultUnsigned {
				t.Fatalf("result = %s, want %s", got, resultUnsigned)
			}
		})
	}
}

func TestVerifiedRequestIsAccepted(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob", testHead))
	rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	check(t, rec, http.StatusAccepted, "godwit apply accepted")
	if len(f.runner.got) != 1 {
		t.Fatalf("runner saw %d commands, want 1", len(f.runner.got))
	}
	cmd := f.runner.got[0]
	if cmd.Name != "apply" || cmd.Login != "alice" || cmd.Head != testHead || cmd.Delivery != "d1" {
		t.Fatalf("command = %+v", cmd)
	}
	if cmd.Source != "github.com/"+testRepo+"@"+testHead {
		t.Fatalf("source = %q", cmd.Source)
	}
	if cmd.Principal.Name != "github:"+testRepo || cmd.Principal.Scope != "pipeline" {
		t.Fatalf("principal = %+v", cmd.Principal)
	}
}

func approvedBy(t *testing.T, login, commit string) *fakeRepo {
	t.Helper()
	repo := writer(t)
	repo.reviews = []Review{{Login: login, State: "APPROVED", CommitID: commit}}

	return repo
}

func TestBodyOverTheLimitIsRefusedByCount(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	f.receiver.cfg.MaxBodyBytes = 16
	rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	check(t, rec, http.StatusRequestEntityTooLarge, "")
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want nothing", rec.Body.String())
	}
	if got := f.result(t); got != resultOversize {
		t.Fatalf("result = %s, want %s", got, resultOversize)
	}
}

func TestUnreadableBody(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	req := httptest.NewRequest(http.MethodPost, Path, &reader{err: errBroken})
	req.Header.Set(eventHeader, EventIssueComment)
	rec := httptest.NewRecorder()
	f.receiver.ServeHTTP(rec, req)
	check(t, rec, http.StatusBadRequest, "")
}

func TestOnlyPostIsAnswered(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	req := httptest.NewRequest(http.MethodGet, Path, stringBody(""))
	req.Header.Set(signatureHeader, sign(testSecret, ""))
	rec := httptest.NewRecorder()
	f.receiver.Handler().ServeHTTP(rec, req)
	check(t, rec, http.StatusMethodNotAllowed, "on POST")
}

func TestDeliveryHeaderIsRequired(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	rec := f.send(t, EventIssueComment, "", "{}", sign(testSecret, "{}"))
	check(t, rec, http.StatusBadRequest, deliveryHeader)
}

func TestUnparsablePayload(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	rec := f.post(t, EventIssueComment, "d1", "not json")
	check(t, rec, http.StatusBadRequest, "not a github payload")
}

func TestReplayedDeliveryEnqueuesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob", testHead))
	body := commentBody("godwit apply", "MEMBER", "alice", now)
	check(t, f.post(t, EventIssueComment, "replayed", body), http.StatusAccepted, "accepted")
	check(t, f.post(t, EventIssueComment, "replayed", body), http.StatusAccepted, "already handled")
	if len(f.runner.got) != 1 {
		t.Fatalf("runner saw %d commands, want 1", len(f.runner.got))
	}
	if f.counts[1].result != resultDuplicate {
		t.Fatalf("second result = %s, want %s", f.counts[1].result, resultDuplicate)
	}
}

func TestUnboundRepositoryGetsNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, event, body string }{
		{"a plan", EventPullRequest, pullBodyJSON("opened", testRepo, testHead)},
		{"an apply", EventIssueComment, commentBody("godwit apply", "MEMBER", "alice", now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, map[string]string{"payments": "someone/else"}, approvedBy(t, "bob", testHead))
			rec := f.post(t, tc.event, "d1", tc.body)
			check(t, rec, http.StatusAccepted, "bound to no godwit target")
			if strings.Contains(rec.Body.String(), "payments") {
				t.Fatalf("the refusal named a target: %q", rec.Body.String())
			}
			if len(f.api.scoped) != 0 || f.store.commits != 0 {
				t.Fatalf("an unbound repository reached github or the store")
			}
			if got := f.result(t); got != resultRefused {
				t.Fatalf("result = %s, want %s", got, resultRefused)
			}
		})
	}
}

func TestRefusalDoesNotSayWhetherTheTargetExists(t *testing.T) {
	t.Parallel()

	b := bind(map[string]string{"orders": testRepo}, testRepo)
	missing := b.Grant(testRepo, "db/migrations", "payments").Error()
	elsewhere := bind(map[string]string{"orders": testRepo, "payments": "other/repo"}, testRepo).
		Grant(testRepo, "db/migrations", "payments").Error()
	if missing != elsewhere {
		t.Fatalf("an unregistered target reads %q and a registered one %q", missing, elsewhere)
	}
}

func TestCommanderWithoutWriteIsRefused(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "bob", testHead)
	repo.perm = map[string]string{"bob": "admin"}
	f := newFixture(t, bound, repo)
	rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	check(t, rec, http.StatusAccepted, `commander alice has permission "none"`)
	if len(f.runner.got) != 0 {
		t.Fatal("a commander without write reached the runner")
	}
}

func TestApprovalMustStandOnTheHead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		reviews []Review
	}{
		{"no review at all", nil},
		{"approval of an older commit", []Review{{Login: "bob", State: "APPROVED", CommitID: testOther}}},
		{"the author's own approval", []Review{{Login: "carol", State: "APPROVED", CommitID: testHead}}},
		{
			"an approval later withdrawn",
			[]Review{{Login: "bob", State: "APPROVED", CommitID: testHead}, {Login: "bob", State: "CHANGES_REQUESTED"}},
		},
		{
			"a dismissed approval",
			[]Review{{Login: "bob", State: "APPROVED", CommitID: testHead}, {Login: "bob", State: "DISMISSED"}},
		},
		{"a comment is not an approval", []Review{{Login: "bob", State: "COMMENTED", CommitID: testHead}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := writer(t)
			repo.reviews = tc.reviews
			f := newFixture(t, bound, repo)
			rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
			check(t, rec, http.StatusAccepted, "no approving review")
			if len(f.runner.got) != 0 {
				t.Fatal("an unapproved apply reached the runner")
			}
		})
	}
}

func TestApprovalOutlivesAWithdrawalItPredates(t *testing.T) {
	t.Parallel()

	repo := writer(t)
	repo.reviews = []Review{
		{Login: "bob", State: "CHANGES_REQUESTED"},
		{Login: "bob", State: "APPROVED", CommitID: testHead},
	}
	f := newFixture(t, bound, repo)
	check(t, f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, "accepted")
}

func TestStaleDeliveryIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob", testHead))
	old := now.Add(-2 * time.Hour)
	rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", old))
	check(t, rec, http.StatusAccepted, "past the 1h0m0s a command may be acted on within")
	if got := f.result(t); got != resultStale {
		t.Fatalf("result = %s, want %s", got, resultStale)
	}
	if len(f.api.scoped) != 0 {
		t.Fatal("a stale delivery reached github")
	}
}

func TestIgnoredDeliveries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, event, body, want string }{
		{
			"an edited comment", EventIssueComment, strings.Replace(
				commentBody("godwit apply", "MEMBER", "alice", now), `"action":"created"`, `"action":"edited"`, 1),
			"not what a comment now says",
		},
		{
			"a comment on an issue", EventIssueComment, strings.Replace(
				commentBody("godwit apply", "MEMBER", "alice", now), `"pull_request":{"url":"x"}`, `"pull_request":null`, 1),
			"not a pull request",
		},
		{
			"prose", EventIssueComment, commentBody("looks good, godwit apply later", "MEMBER", "alice", now),
			"names no godwit command",
		},
		{
			"a review that is not submitted", EventReview, strings.Replace(
				reviewBodyJSON("godwit apply", testHead, now), `"action":"submitted"`, `"action":"dismissed"`, 1),
			"review dismissed ignored",
		},
		{
			"a pull request label", EventPullRequest, pullBodyJSON("labeled", testRepo, testHead),
			"pull request labeled ignored",
		},
		{
			"an event godwit does not act on", "push", `{"repository":{"full_name":"acme/orders"},"installation":{"id":7}}`,
			"does not act on push",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, bound, writer(t))
			check(t, f.post(t, tc.event, "d1", tc.body), http.StatusAccepted, tc.want)
			if got := f.result(t); got != resultIgnored {
				t.Fatalf("result = %s, want %s", got, resultIgnored)
			}
			if f.store.commits != 0 {
				t.Fatal("an ignored delivery was recorded")
			}
		})
	}
}

func TestMalformedCommandIsRefusedNotIgnored(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply --unknown", "MEMBER", "alice", now))
	check(t, rec, http.StatusAccepted, "does not understand '--unknown'")
	if got := f.result(t); got != resultRefused {
		t.Fatalf("result = %s, want %s", got, resultRefused)
	}
}

func TestPayloadsThatNameNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, event, body, want string }{
		{"no repository", EventIssueComment, `{"installation":{"id":7}}`, "names no repository"},
		{
			"a repository that is not owner/repo", EventIssueComment,
			`{"repository":{"full_name":"orders"},"installation":{"id":7}}`, "names no repository",
		},
		{"no installation", EventIssueComment, `{"repository":{"full_name":"acme/orders"}}`, "names no installation"},
		{"a comment with no login", EventIssueComment, strings.Replace(
			commentBody("godwit apply", "MEMBER", "", now), `"login":""`, `"login":"a b"`, 1), "is not a github login"},
		{
			"a comment with no pull request number", EventIssueComment, strings.Replace(
				commentBody("godwit apply", "MEMBER", "alice", now), `"number":3`, `"number":0`, 1),
			"carries no pull request number",
		},
		{
			"a pull request with no head", EventPullRequest, pullBodyJSON("opened", testRepo, "abc"),
			"carries no head commit",
		},
		{
			"a pull request with no number", EventPullRequest, strings.Replace(
				pullBodyJSON("opened", testRepo, testHead), `"number":3`, `"number":0`, 1),
			"carries no pull request number",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, bound, writer(t))
			check(t, f.post(t, tc.event, "d1", tc.body), http.StatusAccepted, tc.want)
		})
	}
}

func TestForkGetsNothing(t *testing.T) {
	t.Parallel()

	t.Run("a fork's pull request is not planned", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, bound, writer(t))
		rec := f.post(t, EventPullRequest, "d1", pullBodyJSON("opened", "fork/orders", testHead))
		check(t, rec, http.StatusAccepted, "a fork's pull request is not planned")
	})
	t.Run("a command on a fork's head is refused", func(t *testing.T) {
		t.Parallel()

		repo := approvedBy(t, "bob", testHead)
		repo.pull.HeadRepo = "fork/orders"
		f := newFixture(t, bound, repo)
		rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
		check(t, rec, http.StatusAccepted, "a fork may not reach the targets")
	})
}

func TestPullRequestPlanCostsNoGitHubCall(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	check(t, f.post(t, EventPullRequest, "d1", pullBodyJSON("synchronize", testRepo, testHead)),
		http.StatusAccepted, "godwit plan accepted")
	if len(f.api.scoped) != 0 {
		t.Fatalf("a plan spent %d github calls", len(f.api.scoped))
	}
	cmd := f.runner.got[0]
	if cmd.Login != "" || cmd.Principal.Scope != "read" || cmd.Comment != nil {
		t.Fatalf("command = %+v", cmd)
	}
}

func TestTheInstallationTokenIsNarrowedToTheRepositoryTheDeliveryNamed(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob", testHead))
	f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	if len(f.api.scoped) != 1 || f.api.scoped[0] != "7/42/"+testRepo {
		t.Fatalf("token scoped %v, want one narrowed to installation 7, repository 42 (%s)", f.api.scoped, testRepo)
	}
}

func TestReviewBodyCommands(t *testing.T) {
	t.Parallel()

	t.Run("on the head it approved", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, bound, approvedBy(t, "bob", testHead))
		check(t, f.post(t, EventReview, "d1", reviewBodyJSON("godwit apply", testHead, now)),
			http.StatusAccepted, "accepted")
	})
	t.Run("on a head that moved", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, bound, approvedBy(t, "bob", testHead))
		check(t, f.post(t, EventReview, "d1", reviewBodyJSON("godwit apply", testOther, now)),
			http.StatusAccepted, "the head moved after the review")
	})
}

func TestFailuresBelowTheReceiverFailClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		break_ func(f *fixture)
	}{
		{"the store cannot be read", func(f *fixture) { f.store.bindErr = errBroken }},
		{"github is unreachable", func(f *fixture) { f.api.err = errBroken }},
		{"the pull request cannot be read", func(f *fixture) { f.api.repo.pullErr = errBroken }},
		{"the permission lookup fails", func(f *fixture) { f.api.repo.permErr = errBroken }},
		{"the reviews cannot be read", func(f *fixture) { f.api.repo.reviewsErr = errBroken }},
		{"the transaction cannot open", func(f *fixture) { f.store.txErr = errBroken }},
		{"the delivery cannot be recorded", func(f *fixture) { f.store.recordErr = errBroken }},
		{"the runner refuses", func(f *fixture) { f.runner.err = errBroken }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, bound, approvedBy(t, "bob", testHead))
			tc.break_(f)
			rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
			check(t, rec, http.StatusInternalServerError, "could not answer this delivery")
			if len(f.runner.got) != 0 {
				t.Fatal("a failure enqueued work anyway")
			}
			if got := f.result(t); got != resultError {
				t.Fatalf("result = %s, want %s", got, resultError)
			}
		})
	}
}

func TestPullRequestHeadIsNotTrustedForACommand(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "bob", testOther)
	repo.pull.Head = testOther
	f := newFixture(t, bound, repo)
	body := strings.Replace(commentBody("godwit apply "+testHead, "MEMBER", "alice", now), "", "", 1)
	check(t, f.post(t, EventIssueComment, "d1", body), http.StatusAccepted, "the head moved after the comment")
}

func TestPullRequestStateGuards(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, command, want string
		mutate              func(pr *PullRequest)
	}{
		{
			"a closed pull request has nothing to apply", "godwit apply", "is closed: nothing to apply",
			func(pr *PullRequest) { pr.State = "closed" },
		},
		{
			"a merged pull request cannot be reverted", "godwit revert", "was merged",
			func(pr *PullRequest) { pr.Merged = true },
		},
		{
			"a head that is not a commit", "godwit apply", "could not read the head",
			func(pr *PullRequest) { pr.Head = "" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := approvedBy(t, "bob", testHead)
			tc.mutate(&repo.pull)
			f := newFixture(t, bound, repo)
			check(t, f.post(t, EventIssueComment, "d1", commentBody(tc.command, "MEMBER", "alice", now)),
				http.StatusAccepted, tc.want)
		})
	}
}

func TestRevertNeedsNoApproval(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	check(t, f.post(t, EventIssueComment, "d1", commentBody("godwit revert", "MEMBER", "alice", now)),
		http.StatusAccepted, "godwit revert accepted")
}

func TestAssociationNarrows(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob", testHead))
	rec := f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "CONTRIBUTOR", "alice", now))
	check(t, rec, http.StatusAccepted, "author association CONTRIBUTOR is not allowed")
	if len(f.api.scoped) != 0 {
		t.Fatal("a refused association still reached github")
	}
}

func TestAnApproverWithoutWriteIsRefused(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "dave", testHead)
	f := newFixture(t, bound, repo)
	check(t, f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, `approver dave has permission "none"`)
}

func TestAnApproverGitHubNamesStrangelyIsRefused(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "not a login", testHead)
	f := newFixture(t, bound, repo)
	check(t, f.post(t, EventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, "is not a github login")
}

func TestPathIsTheOnlyPath(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	req := httptest.NewRequest(http.MethodPost, "/", stringBody("{}"))
	rec := httptest.NewRecorder()
	f.receiver.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestEventLabelIsBounded(t *testing.T) {
	t.Parallel()

	if got := eventLabel(strings.Repeat("x", 5000)); got != "other" {
		t.Fatalf("eventLabel = %q, want other", got)
	}
	if got := eventLabel(EventReview); got != EventReview {
		t.Fatalf("eventLabel = %q, want %s", got, EventReview)
	}
}
