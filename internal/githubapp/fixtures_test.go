package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SamuelMolling/godwit/internal/controlplane"
)

const (
	testSecret = "s3cret"
	testRepo   = "acme/orders"
	testHead   = "1111111111111111111111111111111111111111"
	testOther  = "2222222222222222222222222222222222222222"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeStore struct {
	bindings  map[string]string
	bindErr   error
	txErr     error
	seen      map[string]bool
	recordErr error
	audits    []controlplane.AuditEntry
	auditErr  error
	commits   int
}

func newStore(bindings map[string]string) *fakeStore {
	return &fakeStore{bindings: bindings, seen: map[string]bool{}}
}

func (s *fakeStore) GitHubBindings(context.Context) (map[string]string, error) {
	return s.bindings, s.bindErr
}

func (s *fakeStore) transact(ctx context.Context, fn func(txn) error) error {
	if s.txErr != nil {
		return s.txErr
	}
	if err := fn(s); err != nil {
		return err
	}
	s.commits++

	return nil
}

func (s *fakeStore) RecordDelivery(_ context.Context, id, _, _ string) (bool, error) {
	if s.recordErr != nil {
		return false, s.recordErr
	}
	if s.seen[id] {
		return false, nil
	}
	s.seen[id] = true

	return true, nil
}

func (s *fakeStore) Audit(_ context.Context, e controlplane.AuditEntry) error {
	if s.auditErr != nil {
		return s.auditErr
	}
	s.audits = append(s.audits, e)

	return nil
}

type fakeRepo struct {
	perm          map[string]string
	permErr       error
	pr            pull
	pullErr       error
	submitted     []review
	reviewsErr    error
	reviewsCapped bool
	filesCapped   bool
	touched       []string
	changedErr    error
	files         map[string]string
	fileErr       error
	read          []string
	reacted       []string
	reactErr      error
	notices       []string
	noticeErr     error
	checks        []string
	checkErr      error
}

func (r *fakeRepo) permission(_ context.Context, login string) (string, error) {
	if r.permErr != nil {
		return "", r.permErr
	}
	if p, ok := r.perm[login]; ok {
		return p, nil
	}

	return "none", nil
}

func (r *fakeRepo) pullRequest(context.Context, int) (pull, error) {
	return r.pr, r.pullErr
}

func (r *fakeRepo) reviews(context.Context, int) ([]review, bool, error) {
	return r.submitted, !r.reviewsCapped, r.reviewsErr
}

func (r *fakeRepo) react(_ context.Context, comment int64, reaction string) error {
	r.reacted = append(r.reacted, fmt.Sprintf("%d:%s", comment, reaction))

	return r.reactErr
}

func (r *fakeRepo) speak(_ context.Context, _ int, marker, body string) error {
	r.notices = append(r.notices, marker+"\n"+body)

	return r.noticeErr
}

func (r *fakeRepo) check(_ context.Context, name, head, title, summary string) error {
	r.checks = append(r.checks, fmt.Sprintf("%s@%s %s: %s", name, head, title, summary))

	return r.checkErr
}

func (r *fakeRepo) changed(context.Context, int) (listing, error) {
	return listing{paths: r.touched, listed: len(r.touched), capped: r.filesCapped}, r.changedErr
}

func (r *fakeRepo) file(_ context.Context, path, _ string) ([]byte, error) {
	r.read = append(r.read, path)
	if r.fileErr != nil {
		return nil, r.fileErr
	}
	body, ok := r.files[path]
	if !ok {
		return nil, errAbsent
	}

	return []byte(body), nil
}

type fakeAPI struct {
	repo   *fakeRepo
	err    error
	scoped []string
}

func (a *fakeAPI) repository(_ context.Context, installation, repositoryID int64, repository string) (repoView, error) {
	a.scoped = append(a.scoped, fmt.Sprintf("%d/%d/%s", installation, repositoryID, repository))
	if a.err != nil {
		return nil, a.err
	}

	return a.repo, nil
}

type counted struct{ event, result string }

type fixture struct {
	receiver *Receiver
	store    *fakeStore
	api      *fakeAPI
	counts   []counted
	runner   *fakeRunner
}

type fakeRunner struct {
	got []command
	err error
}

func (r *fakeRunner) enqueue(_ context.Context, _ txn, cmd command) error {
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, cmd)

	return nil
}

func newFixture(t *testing.T, bindings map[string]string, repo *fakeRepo) *fixture {
	t.Helper()
	f := &fixture{store: newStore(bindings), api: &fakeAPI{repo: repo}, runner: &fakeRunner{}}
	r, err := New(Config{
		Secret: testSecret, Store: f.store, API: f.api, Runner: f.runner, Log: testLog,
		Associations: defaultAssociations,
		Record:       func(event, result string) { f.counts = append(f.counts, counted{event, result}) },
		Now:          func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.receiver = r

	return f
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))

	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func (f *fixture) post(t *testing.T, event, delivery, body string) *httptest.ResponseRecorder {
	t.Helper()

	return f.send(t, event, delivery, body, sign(testSecret, body))
}

func (f *fixture) send(t *testing.T, event, delivery, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhookPath, stringBody(body))
	req.Header.Set(eventHeader, event)
	req.Header.Set(deliveryHeader, delivery)
	req.Header.Set(signatureHeader, signature)
	rec := httptest.NewRecorder()
	f.receiver.Handler().ServeHTTP(rec, req)

	return rec
}

func stringBody(s string) io.Reader { return &reader{s: s} }

type reader struct {
	s   string
	i   int
	err error
}

func (r *reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n

	return n, nil
}

var errBroken = errors.New("broken")

func eventAt(now time.Time) string { return now.UTC().Format(time.RFC3339) }

func commentBody(body, association, login string, at time.Time) string {
	return fmt.Sprintf(`{"action":"created","repository":{"id":42,"full_name":%q},"installation":{"id":7},
		"issue":{"number":3,"pull_request":{"url":"x"}},
		"comment":{"body":%q,"created_at":%q,"user":{"login":%q},"author_association":%q}}`,
		testRepo, body, eventAt(at), login, association)
}

func reviewBodyJSON(body, commitID string, at time.Time) string {
	return fmt.Sprintf(`{"action":"submitted","repository":{"id":42,"full_name":%q},"installation":{"id":7},
		"pull_request":{"number":3},
		"review":{"body":%q,"commit_id":%q,"submitted_at":%q,"user":{"login":"alice"},"author_association":"MEMBER"}}`,
		testRepo, body, commitID, eventAt(at))
}

func pullBodyJSON(action, headRepo, head string) string {
	return fmt.Sprintf(`{"action":%q,"repository":{"id":42,"full_name":%q},"installation":{"id":7},
		"pull_request":{"number":3,"head":{"sha":%q,"repo":{"full_name":%q}}}}`,
		action, testRepo, head, headRepo)
}

func writer(t *testing.T) *fakeRepo {
	t.Helper()

	return &fakeRepo{
		perm:    map[string]string{"alice": "write", "bob": "admin"},
		pr:      pull{head: testHead, headRepo: testRepo, state: "open", author: "carol"},
		touched: []string{"db/migrations/20260101000000_x.up.sql"},
		files:   map[string]string{"godwit.yaml": "dir: db/migrations\ntarget: orders\n"},
	}
}

var now = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
