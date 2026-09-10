package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testKey = mustKey()

func mustKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return key
}

type brokenSigner struct{ crypto.Signer }

func (brokenSigner) Public() crypto.PublicKey { return testKey.Public() }

func (brokenSigner) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errBroken
}

func newClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &Client{
		BaseURL: srv.URL, AppID: "12345", Signer: testKey,
		HTTP: srv.Client(), Now: func() time.Time { return now },
	}
}

func TestInstallationTokenIsNarrowedToOneRepository(t *testing.T) {
	t.Parallel()

	var gotBody, gotPath, gotAuth string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody, gotPath, gotAuth = string(body), r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"token":"ghs_installation"}`)
	})
	repo, err := c.repository(context.Background(), 7, 42, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"repository_ids":[42]}` {
		t.Fatalf("body = %s", gotBody)
	}
	if gotPath != "/app/installations/7/access_tokens" {
		t.Fatalf("path = %s", gotPath)
	}
	claims := jwtClaims(t, strings.TrimPrefix(gotAuth, "Bearer "))
	if claims["iss"] != "12345" || claims["iat"] != float64(now.Add(-time.Minute).Unix()) {
		t.Fatalf("claims = %v", claims)
	}
	if r, ok := repo.(*repoClient); !ok || r.token != "ghs_installation" {
		t.Fatalf("repo = %+v", repo)
	}
}

func jwtClaims(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", jwt)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}

	return claims
}

func TestInstallationTokenFailures(t *testing.T) {
	t.Parallel()

	t.Run("github refuses", func(t *testing.T) {
		t.Parallel()

		c := newClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
		if _, err := c.repository(context.Background(), 7, 42, testRepo); err == nil ||
			!strings.Contains(err.Error(), "mint installation token") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("github answers with no token", func(t *testing.T) {
		t.Parallel()

		c := newClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
		if _, err := c.repository(context.Background(), 9, 43, "acme/payments"); err == nil ||
			!strings.Contains(err.Error(), "no token") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the app key cannot sign", func(t *testing.T) {
		t.Parallel()

		c := newClient(t, func(http.ResponseWriter, *http.Request) {})
		c.Signer = brokenSigner{}
		if _, err := c.repository(context.Background(), 7, 42, testRepo); err == nil ||
			!strings.Contains(err.Error(), "sign app jwt") {
			t.Fatalf("err = %v", err)
		}
	})
}

func testRepoClient(t *testing.T, h http.HandlerFunc) *repoClient {
	t.Helper()

	return &repoClient{client: newClient(t, h), token: "ghs_x", repository: testRepo}
}

func TestPermission(t *testing.T) {
	t.Parallel()

	t.Run("a collaborator", func(t *testing.T) {
		t.Parallel()

		r := testRepoClient(t, func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/repos/"+testRepo+"/collaborators/alice/permission" {
				t.Errorf("path = %s", req.URL.Path)
			}
			_, _ = io.WriteString(w, `{"permission":"write"}`)
		})
		perm, err := r.permission(context.Background(), "alice")
		if err != nil || perm != "write" {
			t.Fatalf("permission = %q, %v", perm, err)
		}
	})
	t.Run("a login github does not know is none, not an error", func(t *testing.T) {
		t.Parallel()

		r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		perm, err := r.permission(context.Background(), "stranger")
		if err != nil || perm != "none" {
			t.Fatalf("permission = %q, %v", perm, err)
		}
	})
	t.Run("a lookup that fails refuses rather than allows", func(t *testing.T) {
		t.Parallel()

		r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
		if _, err := r.permission(context.Background(), "alice"); err == nil {
			t.Fatal("a failed permission lookup returned no error")
		}
	})
}

func TestPullRequestRead(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"state":"open","merged":true,"user":{"login":"carol"},
			"head":{"sha":"`+testHead+`","repo":{"full_name":"`+testRepo+`"}}}`)
	})
	pr, err := r.pullRequest(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := pull{head: testHead, headRepo: testRepo, state: "open", merged: true, author: "carol"}
	if pr != want {
		t.Fatalf("PullRequest = %+v, want %+v", pr, want)
	}
}

func TestPullRequestReadFails(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if _, err := r.pullRequest(context.Background(), 3); err == nil {
		t.Fatal("no error")
	}
}

func TestReviewsFollowThePages(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+srv.URL+`/next?page=2>; rel="next", <x>; rel="last"`)
			_, _ = io.WriteString(w, `[{"state":"COMMENTED","commit_id":"a","user":{"login":"bob"}}]`)

			return
		}
		_, _ = io.WriteString(w, `[{"state":"APPROVED","commit_id":"`+testHead+`","user":{"login":"bob"}}]`)
	}))
	t.Cleanup(srv.Close)
	r := &repoClient{
		client: &Client{BaseURL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }},
		token:  "ghs_x", repository: testRepo,
	}
	reviews, whole, err := r.reviews(context.Background(), 3)
	if err != nil || !whole {
		t.Fatalf("reviews = %t, %v", whole, err)
	}
	if len(reviews) != 2 || reviews[1].state != "APPROVED" || reviews[1].commitID != testHead {
		t.Fatalf("reviews = %+v", reviews)
	}
}

func TestReviewsFail(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, _, err := r.reviews(context.Background(), 3); err == nil {
		t.Fatal("no error")
	}
}

func TestNextPage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ link, want string }{
		{"", ""},
		{`<https://x/2>; rel="next"`, "https://x/2"},
		{`<https://x/1>; rel="prev", <https://x/3>; rel="next"`, "https://x/3"},
		{`<https://x/9>; rel="last"`, ""},
		{`https://x/2; rel="next"`, ""},
		{`no semicolon`, ""},
	} {
		if got := nextPage(tc.link); got != tc.want {
			t.Fatalf("nextPage(%q) = %q, want %q", tc.link, got, tc.want)
		}
	}
}

func TestCallFailures(t *testing.T) {
	t.Parallel()

	t.Run("a base url that is not one", func(t *testing.T) {
		t.Parallel()

		c := &Client{BaseURL: "http://[::1", HTTP: http.DefaultClient, Now: func() time.Time { return now }}
		r := &repoClient{client: c, token: "x", repository: testRepo}
		if _, err := r.pullRequest(context.Background(), 3); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("github unreachable", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		r := &repoClient{
			client: &Client{BaseURL: url, HTTP: http.DefaultClient, Now: func() time.Time { return now }},
			token:  "x", repository: testRepo,
		}
		if _, err := r.pullRequest(context.Background(), 3); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("an answer that cuts off mid-body", func(t *testing.T) {
		t.Parallel()

		r := &repoClient{
			client: &Client{
				BaseURL: "http://github.test", Now: func() time.Time { return now },
				HTTP: &http.Client{Transport: brokenBody{}},
			},
			token: "x", repository: testRepo,
		}
		if _, err := r.pullRequest(context.Background(), 3); !errors.Is(err, errBroken) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an answer that is not json", func(t *testing.T) {
		t.Parallel()

		r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>") })
		if _, err := r.pullRequest(context.Background(), 3); err == nil {
			t.Fatal("no error")
		}
	})
}

type brokenBody struct{}

func (brokenBody) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(&reader{err: errBroken}),
		Header:     http.Header{},
	}, nil
}

func TestChangedFilesFollowThePages(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+srv.URL+`/next?page=2>; rel="next"`)
			_, _ = io.WriteString(w, `[{"filename":"db/migrations/a.up.sql"}]`)

			return
		}
		_, _ = io.WriteString(w, `[{"filename":"db/migrations/c.up.sql","previous_filename":"db/old/c.up.sql"}]`)
	}))
	t.Cleanup(srv.Close)
	r := &repoClient{
		client: &Client{BaseURL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }},
		token:  "ghs_x", repository: testRepo,
	}
	got, err := r.changed(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"db/migrations/a.up.sql", "db/migrations/c.up.sql", "db/old/c.up.sql"}
	if strings.Join(got.paths, ",") != strings.Join(want, ",") {
		t.Fatalf("changed = %v, want %v", got.paths, want)
	}
	if got.listed != 2 || !got.whole(2) {
		t.Fatalf("listing = %+v", got)
	}
}

func TestChangedFilesFail(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := r.changed(context.Background(), 3); err == nil {
		t.Fatal("no error")
	}
}

func TestFileAtACommit(t *testing.T) {
	t.Parallel()

	var gotURL string
	r := testRepoClient(t, func(w http.ResponseWriter, req *http.Request) {
		gotURL = req.URL.String()
		_, _ = io.WriteString(w, `{"type":"file","size":9,"encoding":"base64","content":"dGFyZ2V0OiB4\n"}`)
	})
	body, err := r.file(context.Background(), "godwit.yaml", testHead)
	if err != nil || string(body) != "target: x" {
		t.Fatalf("file = %q, %v", body, err)
	}
	if gotURL != "/repos/"+testRepo+"/contents/godwit.yaml?ref="+testHead {
		t.Fatalf("url = %s", gotURL)
	}
}

func TestFileRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, answer, want string }{
		{"a directory", `{"type":"dir"}`, "is a dir, not a file"},
		{"nothing godwit understands", `{"type":""}`, "is a none, not a file"},
		{"too large", `{"type":"file","size":999999}`, "over the"},
		{"another encoding", `{"type":"file","size":1,"encoding":"none"}`, "came back none-encoded"},
		{"content that is not base64", `{"type":"file","size":1,"encoding":"base64","content":"!!"}`, "godwit.yaml:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.answer) })
			_, err := r.file(context.Background(), "godwit.yaml", testHead)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFileAbsentIsNotAFailure(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err := r.file(context.Background(), "godwit.yaml", testHead); !errors.Is(err, errAbsent) {
		t.Fatalf("err = %v, want errAbsent", err)
	}
}

func TestFileTransportFailure(t *testing.T) {
	t.Parallel()

	r := testRepoClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := r.file(context.Background(), "godwit.yaml", testHead); err == nil {
		t.Fatal("no error")
	}
}

func pages(t *testing.T, entry string, n int) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	page := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page++
		w.Header().Set("Link", `<`+srv.URL+`/next?page=`+strconv.Itoa(page+1)+`>; rel="next"`)
		items := make([]string, n)
		for i := range items {
			items[i] = entry
		}
		_, _ = io.WriteString(w, "["+strings.Join(items, ",")+"]")
	}))
	t.Cleanup(srv.Close)

	return srv
}

func clientAt(t *testing.T, srv *httptest.Server) *repoClient {
	t.Helper()

	return &repoClient{
		client: &Client{BaseURL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }},
		token:  "ghs_x", repository: testRepo,
	}
}

func TestAChangedFileListingStopsAtTheCapAndSaysSo(t *testing.T) {
	t.Parallel()

	r := clientAt(t, pages(t, `{"filename":"a.sql"}`, 100))
	got, err := r.changed(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.listed != filesCap || !got.capped || got.whole(0) {
		t.Fatalf("listing = %d entries, capped %t, whole %t", got.listed, got.capped, got.whole(0))
	}
}

func TestAReviewListingStopsAtTheCapAndDecidesNothing(t *testing.T) {
	t.Parallel()

	r := clientAt(t, pages(t, `{"state":"APPROVED","user":{"login":"bob"}}`, 100))
	got, whole, err := r.reviews(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if whole || got != nil {
		t.Fatalf("reviews = %d, whole %t; want nothing and not whole", len(got), whole)
	}
}

func TestWholeReadsTheCountItWasGiven(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		l     listing
		files int
		want  bool
	}{
		{listing{listed: 3}, 0, true},
		{listing{listed: 3}, 3, true},
		{listing{listed: 3}, 4, false},
		{listing{listed: 3, capped: true}, 3, false},
	} {
		if got := tc.l.whole(tc.files); got != tc.want {
			t.Fatalf("%+v.whole(%d) = %t, want %t", tc.l, tc.files, got, tc.want)
		}
	}
}
