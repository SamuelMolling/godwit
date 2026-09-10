package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	testRepo  = "acme/orders"
	testHead  = "1111111111111111111111111111111111111111"
	testOther = "2222222222222222222222222222222222222222"
)

type fakeRepo struct {
	perm     map[string]string
	permErr  error
	pr       PullRequest
	prErr    error
	reviews  []Review
	partial  bool
	reviewer error
}

func (r *fakeRepo) Permission(_ context.Context, login string) (string, error) {
	if r.permErr != nil {
		return "", r.permErr
	}
	if p, ok := r.perm[login]; ok {
		return p, nil
	}

	return "none", nil
}

func (r *fakeRepo) PullRequest(context.Context, int) (PullRequest, error) { return r.pr, r.prErr }

func (r *fakeRepo) Reviews(context.Context, int) ([]Review, bool, error) {
	return r.reviews, !r.partial, r.reviewer
}

func newRepo() *fakeRepo {
	return &fakeRepo{
		perm: map[string]string{"alice": "write", "bob": "admin"},
		pr:   PullRequest{Head: testHead, HeadRepo: testRepo, State: "open", Files: 3},
	}
}

func command() Command {
	return Command{Name: "plan", Repository: testRepo, Commander: "alice", Association: "MEMBER", Number: 7}
}

var errBoom = errors.New("boom")

func TestForgeAuthorizeAdmitsACommanderWithWrite(t *testing.T) {
	t.Parallel()

	f, err := NewForge(nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := newRepo()
	pr, ref, err := f.Authorize(context.Background(), opens(repo), command())
	if err != nil || ref != "" {
		t.Fatalf("authorize = %+v %v", ref, err)
	}
	if pr.Head != testHead || pr.Files != 3 {
		t.Fatalf("pull request = %+v", pr)
	}
}

func TestForgeAuthorizeAdmitsAnApplyAnApproverWithWriteStandsBehind(t *testing.T) {
	t.Parallel()

	repo, c := newRepo(), command()
	c.Name = "apply"
	repo.reviews = []Review{{Login: "bob", State: "APPROVED"}}
	if _, ref, err := newForge(t).Authorize(context.Background(), opens(repo), c); ref != "" || err != nil {
		t.Fatalf("authorize = %+v %v", ref, err)
	}
}

func TestForgeAuthorizeRefuses(t *testing.T) {
	t.Parallel()

	approved := []Review{{Login: "bob", State: "APPROVED"}}
	for _, tc := range []struct {
		name string
		with func(*fakeRepo, *Command)
		want string
	}{
		{"an association nobody granted", func(_ *fakeRepo, c *Command) { c.Association = "" }, "author association none is not allowed"},
		{"a commander without write", func(r *fakeRepo, _ *Command) { delete(r.perm, "alice") }, "commander alice has permission"},
		{"a head godwit cannot read", func(r *fakeRepo, _ *Command) { r.pr.Head = "" }, "could not read the head"},
		{"a fork", func(r *fakeRepo, _ *Command) { r.pr.HeadRepo = "fork/orders" }, "a fork may not reach"},
		{"a closed pull request", func(r *fakeRepo, _ *Command) { r.pr.State = "closed" }, "nothing to plan"},
		{"a merged one to revert", func(r *fakeRepo, c *Command) { r.pr.Merged, c.Name = true, "revert" }, "belong to the base branch"},
		{"a review the head outran", func(_ *fakeRepo, c *Command) { c.ReviewSHA = testOther }, "the head moved after the review"},
		{"a comment naming another commit", func(_ *fakeRepo, c *Command) { c.CommentSHA = "abcdef" }, "the head moved after the comment"},
		{"an apply with no approval", func(_ *fakeRepo, c *Command) { c.Name = "apply" }, "no approving review standing"},
		{
			"an apply whose approval was withdrawn",
			func(r *fakeRepo, c *Command) {
				c.Name = "apply"
				r.reviews = append(approved, Review{Login: "bob", State: "CHANGES_REQUESTED"},
					Review{Login: "carol", State: "COMMENTED"})
			},
			"no approving review standing",
		},
		{
			"an apply read off a truncated review list",
			func(r *fakeRepo, c *Command) { c.Name, r.reviews, r.partial = "apply", approved, true },
			"more reviews than godwit reads",
		},
		{
			"an approver github names oddly",
			func(r *fakeRepo, c *Command) {
				c.Name, r.reviews = "apply", []Review{{Login: "a b", State: "APPROVED"}}
			},
			"is not a github login",
		},
		{
			"an approver without write",
			func(r *fakeRepo, c *Command) {
				c.Name, r.reviews = "apply", []Review{{Login: "carol", State: "APPROVED"}}
			},
			"approver carol has permission",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo, c := newRepo(), command()
			tc.with(repo, &c)
			_, ref, err := newForge(t).Authorize(context.Background(), opens(repo), c)
			if err != nil || !strings.Contains(ref, tc.want) {
				t.Fatalf("authorize = %q %v, want a refusal carrying %q", ref, err, tc.want)
			}
		})
	}
}

func TestForgeAuthorizeReportsWhatItCouldNotRead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		with func(*fakeRepo)
		open func(context.Context) (Repository, error)
	}{
		{"the installation token", nil, func(context.Context) (Repository, error) { return nil, errBoom }},
		{"the permission", func(r *fakeRepo) { r.permErr = errBoom }, nil},
		{"the pull request", func(r *fakeRepo) { r.prErr = errBoom }, nil},
		{"the reviews", func(r *fakeRepo) { r.reviewer = errBoom }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo, c := newRepo(), command()
			c.Name = "apply"
			if tc.with != nil {
				tc.with(repo)
			}
			open := tc.open
			if open == nil {
				open = opens(repo)
			}
			if _, _, err := newForge(t).Authorize(context.Background(), open, c); !errors.Is(err, errBoom) {
				t.Fatalf("authorize = %v, want the read error", err)
			}
		})
	}
}

func TestNewForge(t *testing.T) {
	t.Parallel()

	f := newForge(t)
	if !f.associations["MEMBER"] || f.associations["NONE"] || len(f.associations) != 3 {
		t.Fatalf("default associations = %v", f.associations)
	}
	mixed, err := NewForge([]string{" owner ", "MEMBER"})
	if err != nil {
		t.Fatal(err)
	}
	if !mixed.associations["OWNER"] || !mixed.associations["MEMBER"] || len(mixed.associations) != 2 {
		t.Fatalf("associations = %v", mixed.associations)
	}
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"OWNER", "CONTRIBUTOR"}, "CONTRIBUTOR is not access"},
		{[]string{"WRITER"}, "unknown author association"},
		{[]string{" ", ""}, "no comment could ever command"},
	} {
		if _, err := NewForge(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("NewForge(%q) = %v, want it to carry %q", tc.in, err, tc.want)
		}
	}
}

func TestForgeShapeHelpers(t *testing.T) {
	t.Parallel()

	if short("abc") != "abc" || short(testHead) != "1111111" {
		t.Fatal("short")
	}
	if orNone("open") != "open" || orNone("") != "none" {
		t.Fatal("orNone")
	}
	if validLogin("") || !validLogin("alice") {
		t.Fatal("validLogin")
	}
	if validSHA("abc") || !validSHA(testHead) {
		t.Fatal("validSHA")
	}
}

func newForge(t *testing.T) Forge {
	t.Helper()
	f, err := NewForge(nil)
	if err != nil {
		t.Fatal(err)
	}

	return f
}

func opens(repo Repository) func(context.Context) (Repository, error) {
	return func(context.Context) (Repository, error) { return repo, nil }
}
