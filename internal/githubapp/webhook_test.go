package githubapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SamuelMolling/godwit/internal/authz"
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
			rec := f.send(t, eventIssueComment, "d1", body, tc.signature)
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

	f := newFixture(t, bound, approvedBy(t, "bob"))
	rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	check(t, rec, http.StatusAccepted, "godwit apply accepted")
	if len(f.runner.got) != 1 {
		t.Fatalf("runner saw %d commands, want 1", len(f.runner.got))
	}
	cmd := f.runner.got[0]
	if cmd.name != "apply" || cmd.login != "alice" || cmd.head != testHead || cmd.delivery != "d1" {
		t.Fatalf("command = %+v", cmd)
	}
	if cmd.source != "github.com/"+testRepo+"@"+testHead {
		t.Fatalf("source = %q", cmd.source)
	}
	if cmd.principal.Name != "github:"+testRepo || cmd.principal.Scope != "pipeline" {
		t.Fatalf("principal = %+v", cmd.principal)
	}
}

func approvedBy(t *testing.T, login string) *fakeRepo {
	t.Helper()
	repo := writer(t)
	repo.submitted = []authz.Review{{Login: login, State: "APPROVED"}}

	return repo
}

func TestBodyOverTheLimitIsRefusedByCount(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	f.receiver.cfg.MaxBodyBytes = 16
	rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
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
	req := httptest.NewRequest(http.MethodPost, webhookPath, &reader{err: errBroken})
	req.Header.Set(eventHeader, eventIssueComment)
	rec := httptest.NewRecorder()
	f.receiver.serve(rec, req)
	check(t, rec, http.StatusBadRequest, "")
}

func TestOnlyPostIsAnswered(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	req := httptest.NewRequest(http.MethodGet, webhookPath, stringBody(""))
	req.Header.Set(signatureHeader, sign(testSecret, ""))
	rec := httptest.NewRecorder()
	f.receiver.Handler().ServeHTTP(rec, req)
	check(t, rec, http.StatusMethodNotAllowed, "on POST")
}

func TestDeliveryHeaderIsRequired(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	rec := f.send(t, eventIssueComment, "", "{}", sign(testSecret, "{}"))
	check(t, rec, http.StatusBadRequest, deliveryHeader)
}

func TestUnparsablePayload(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	rec := f.post(t, eventIssueComment, "d1", "not json")
	check(t, rec, http.StatusBadRequest, "not a github payload")
}

func TestReplayedDeliveryEnqueuesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob"))
	body := commentBody("godwit apply", "MEMBER", "alice", now)
	check(t, f.post(t, eventIssueComment, "replayed", body), http.StatusAccepted, "accepted")
	check(t, f.post(t, eventIssueComment, "replayed", body), http.StatusAccepted, "already handled")
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
		{"a plan", eventPullRequest, pullBodyJSON("opened", testRepo, testHead)},
		{"an apply", eventIssueComment, commentBody("godwit apply", "MEMBER", "alice", now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := approvedBy(t, "bob")
			f := newFixture(t, map[string]string{"payments": "someone/else"}, repo)
			rec := f.post(t, tc.event, "d1", tc.body)
			check(t, rec, http.StatusAccepted, "bound to no godwit target")
			if strings.Contains(rec.Body.String(), "payments") {
				t.Fatalf("the refusal named a target: %q", rec.Body.String())
			}
			if f.store.commits != 0 || len(f.runner.got) != 0 {
				t.Fatal("an unbound repository was acted on")
			}
			if len(repo.notices) != 1 || !strings.Contains(repo.notices[0], "bound to no godwit target") {
				t.Fatalf("notices = %v, want the refusal said once on the pull request", repo.notices)
			}
			if len(repo.read) != 0 || len(repo.reacted) != 0 {
				t.Fatal("an unbound repository was read as well as told")
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
	missing := b.grant(testRepo, "db/migrations", "payments").Error()
	elsewhere := bind(map[string]string{"orders": testRepo, "payments": "other/repo"}, testRepo).
		grant(testRepo, "db/migrations", "payments").Error()
	if missing != elsewhere {
		t.Fatalf("an unregistered target reads %q and a registered one %q", missing, elsewhere)
	}
}

func TestCommanderWithoutWriteIsRefused(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "bob")
	repo.perm = map[string]string{"bob": "admin"}
	f := newFixture(t, bound, repo)
	rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	check(t, rec, http.StatusAccepted, `commander alice has permission "none"`)
	if len(f.runner.got) != 0 {
		t.Fatal("a commander without write reached the runner")
	}
}

func TestGodwitRefusesWhatGitHubDoesNotCallApproved(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		reviews []authz.Review
	}{
		{"no review at all", nil},
		{
			"an approval later withdrawn",
			[]authz.Review{{Login: "bob", State: "APPROVED"}, {Login: "bob", State: "CHANGES_REQUESTED"}},
		},
		{
			"a dismissed approval",
			[]authz.Review{{Login: "bob", State: "APPROVED"}, {Login: "bob", State: "DISMISSED"}},
		},
		{"a comment is not an approval", []authz.Review{{Login: "bob", State: "COMMENTED"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := writer(t)
			repo.submitted = tc.reviews
			f := newFixture(t, bound, repo)
			rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
			check(t, rec, http.StatusAccepted, "github reports no approving review")
			if len(f.runner.got) != 0 {
				t.Fatal("an unapproved apply reached the runner")
			}
		})
	}
}

func TestApprovalOutlivesAWithdrawalItPredates(t *testing.T) {
	t.Parallel()

	repo := writer(t)
	repo.submitted = []authz.Review{
		{Login: "bob", State: "CHANGES_REQUESTED"},
		{Login: "bob", State: "APPROVED"},
	}
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, "accepted")
}

func TestStaleDeliveryIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob"))
	old := now.Add(-2 * time.Hour)
	rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", old))
	check(t, rec, http.StatusAccepted, "past the 1h0m0s a command may be acted on within")
	if got := f.result(t); got != resultStale {
		t.Fatalf("result = %s, want %s", got, resultStale)
	}
	if len(f.api.repo.notices) != 1 {
		t.Fatalf("notices = %v, want the commenter told why nothing happened", f.api.repo.notices)
	}
	if len(f.api.repo.reacted) != 0 || f.store.commits != 0 {
		t.Fatal("a stale delivery was acted on")
	}
}

func TestIgnoredDeliveries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, event, body, want string }{
		{
			"an edited comment", eventIssueComment, strings.Replace(
				commentBody("godwit apply", "MEMBER", "alice", now), `"action":"created"`, `"action":"edited"`, 1),
			"not what a comment now says",
		},
		{
			"a comment on an issue", eventIssueComment, strings.Replace(
				commentBody("godwit apply", "MEMBER", "alice", now), `"pull_request":{"url":"x"}`, `"pull_request":null`, 1),
			"not a pull request",
		},
		{
			"prose", eventIssueComment, commentBody("looks good, godwit apply later", "MEMBER", "alice", now),
			"names no godwit command",
		},
		{
			"a review that is not submitted", eventReview, strings.Replace(
				reviewBodyJSON(testHead, now), `"action":"submitted"`, `"action":"dismissed"`, 1),
			"review dismissed ignored",
		},
		{
			"a pull request label", eventPullRequest, pullBodyJSON("labeled", testRepo, testHead),
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
	rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply --unknown", "MEMBER", "alice", now))
	check(t, rec, http.StatusAccepted, "does not understand '--unknown'")
	if got := f.result(t); got != resultRefused {
		t.Fatalf("result = %s, want %s", got, resultRefused)
	}
}

func TestPayloadsThatNameNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, event, body, want string }{
		{"no repository", eventIssueComment, `{"installation":{"id":7}}`, "names no repository"},
		{
			"a repository that is not owner/repo", eventIssueComment,
			`{"repository":{"full_name":"orders"},"installation":{"id":7}}`, "names no repository",
		},
		{"no installation", eventIssueComment, `{"repository":{"full_name":"acme/orders"}}`, "names no installation"},
		{"a comment with no login", eventIssueComment, strings.Replace(
			commentBody("godwit apply", "MEMBER", "", now), `"login":""`, `"login":"a b"`, 1), "is not a github login"},
		{
			"a comment with no pull request number", eventIssueComment, strings.Replace(
				commentBody("godwit apply", "MEMBER", "alice", now), `"number":3`, `"number":0`, 1),
			"carries no pull request number",
		},
		{
			"a pull request with no head", eventPullRequest, pullBodyJSON("opened", testRepo, "abc"),
			"carries no head commit",
		},
		{
			"a pull request with no number", eventPullRequest, strings.Replace(
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
		rec := f.post(t, eventPullRequest, "d1", pullBodyJSON("opened", "fork/orders", testHead))
		check(t, rec, http.StatusAccepted, "a fork may not reach the targets")
	})
	t.Run("a command on a fork's head is refused", func(t *testing.T) {
		t.Parallel()

		repo := approvedBy(t, "bob")
		repo.pr.HeadRepo = "fork/orders"
		f := newFixture(t, bound, repo)
		rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
		check(t, rec, http.StatusAccepted, "a fork may not reach the targets")
	})
}

func TestPullRequestPlansTheProjectItTouched(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	check(t, f.post(t, eventPullRequest, "d1", pullBodyJSON("synchronize", testRepo, testHead)),
		http.StatusAccepted, "godwit plan accepted at 1111111 for orders")
	cmd := f.runner.got[0]
	if cmd.login != "" || cmd.principal.Scope != "read" || cmd.cmd != nil {
		t.Fatalf("command = %+v", cmd)
	}
	if len(cmd.projects) != 1 || cmd.projects[0].target != "orders" || cmd.projects[0].dir != "db/migrations" {
		t.Fatalf("projects = %+v", cmd.projects)
	}
}

func TestTheInstallationTokenIsNarrowedToTheRepositoryTheDeliveryNamed(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob"))
	f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
	if len(f.api.scoped) != 1 || f.api.scoped[0] != "7/42/"+testRepo {
		t.Fatalf("token scoped %v, want one narrowed to installation 7, repository 42 (%s)", f.api.scoped, testRepo)
	}
}

func TestReviewBodyCommands(t *testing.T) {
	t.Parallel()

	t.Run("on the head it approved", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, bound, approvedBy(t, "bob"))
		check(t, f.post(t, eventReview, "d1", reviewBodyJSON(testHead, now)),
			http.StatusAccepted, "accepted")
	})
	t.Run("on a head that moved", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, bound, approvedBy(t, "bob"))
		check(t, f.post(t, eventReview, "d1", reviewBodyJSON(testOther, now)),
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

			f := newFixture(t, bound, approvedBy(t, "bob"))
			tc.break_(f)
			rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now))
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

	repo := approvedBy(t, "bob")
	repo.pr.Head = testOther
	f := newFixture(t, bound, repo)
	body := strings.Replace(commentBody("godwit apply "+testHead, "MEMBER", "alice", now), "", "", 1)
	check(t, f.post(t, eventIssueComment, "d1", body), http.StatusAccepted, "the head moved after the comment")
}

func TestPullRequestStateGuards(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, command, want string
		mutate              func(pr *authz.PullRequest)
	}{
		{
			"a closed pull request has nothing to apply", "godwit apply", "is closed: nothing to apply",
			func(pr *authz.PullRequest) { pr.State = "closed" },
		},
		{
			"a merged pull request cannot be reverted", "godwit revert", "was merged",
			func(pr *authz.PullRequest) { pr.Merged = true },
		},
		{
			"a head that is not a commit", "godwit apply", "could not read the head",
			func(pr *authz.PullRequest) { pr.Head = "" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := approvedBy(t, "bob")
			tc.mutate(&repo.pr)
			f := newFixture(t, bound, repo)
			check(t, f.post(t, eventIssueComment, "d1", commentBody(tc.command, "MEMBER", "alice", now)),
				http.StatusAccepted, tc.want)
		})
	}
}

func TestRevertNeedsNoApproval(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, writer(t))
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit revert", "MEMBER", "alice", now)),
		http.StatusAccepted, "godwit revert accepted")
}

func TestAssociationNarrows(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob"))
	rec := f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "CONTRIBUTOR", "alice", now))
	check(t, rec, http.StatusAccepted, "author association CONTRIBUTOR is not allowed")
	if len(f.api.repo.reacted) != 0 {
		t.Fatal("godwit acknowledged a comment it would not read as a command")
	}
	if len(f.api.repo.notices) != 1 {
		t.Fatalf("notices = %v, want the commenter told why", f.api.repo.notices)
	}
}

func TestAnApproverWithoutWriteIsRefused(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "dave")
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, `approver dave has permission "none"`)
}

func TestAnApproverGitHubNamesStrangelyIsRefused(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "not a login")
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
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
	if got := eventLabel(eventReview); got != eventReview {
		t.Fatalf("eventLabel = %q, want %s", got, eventReview)
	}
}

func TestAnApprovalGitHubStillShowsSurvivesAPush(t *testing.T) {
	t.Parallel()

	repo := approvedBy(t, "bob")
	repo.pr.Head = testOther
	f := newFixture(t, bound, repo)
	check(t, f.post(t, eventIssueComment, "d1", commentBody("godwit apply", "MEMBER", "alice", now)),
		http.StatusAccepted, "godwit apply accepted")
}

func TestTheReviewThatCommandedIsStillCheckedAgainstTheHead(t *testing.T) {
	t.Parallel()

	f := newFixture(t, bound, approvedBy(t, "bob"))
	check(t, f.post(t, eventReview, "d1", reviewBodyJSON(testOther, now)),
		http.StatusAccepted, "the head moved after the review")
}
