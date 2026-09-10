package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

const (
	webhookPath     = "/github/webhook"
	signatureHeader = "X-Hub-Signature-256"
	deliveryHeader  = "X-GitHub-Delivery"
	eventHeader     = "X-GitHub-Event"
	signaturePrefix = "sha256="
)

const (
	defaultMaxBodyBytes = 1 << 20
	defaultMaxAge       = time.Hour
	// defaultReaction is on where Atlantis's --emoji-reaction is off; decision 0018 argues why.
	defaultReaction = "eyes"
)

const (
	resultAccepted  = "accepted"
	resultDuplicate = "duplicate"
	resultIgnored   = "ignored"
	resultStale     = "stale"
	resultRefused   = "refused"
	resultUnsigned  = "unsigned"
	resultOversize  = "oversize"
	resultMalformed = "malformed"
	resultError     = "error"
)

type txn interface {
	RecordDelivery(ctx context.Context, id, event, repository string) (bool, error)
	Audit(ctx context.Context, e controlplane.AuditEntry) error
}

type store interface {
	GitHubBindings(ctx context.Context) (map[string]string, error)
	transact(ctx context.Context, fn func(txn) error) error
}

type storeAdapter struct{ *controlplane.Store }

func (s storeAdapter) transact(ctx context.Context, fn func(txn) error) error {
	return s.Transact(ctx, func(tx *controlplane.Store) error { return fn(tx) })
}

// Adapt returns the receiver's view of the control-plane store, for Config.Store.
func Adapt(s *controlplane.Store) store { return storeAdapter{s} }

// Config is everything the receiver needs to answer a delivery; New fills in what is left zero.
type Config struct {
	Secret       string
	MaxBodyBytes int
	MaxAge       time.Duration
	Associations []string
	// Reaction is the emoji godwit adds to a comment it read as a command; empty takes the default, NoReaction adds none.
	Reaction string
	Store    store
	API      forge
	Runner   runner
	Record   func(event, result string)
	Log      *slog.Logger
	Now      func() time.Time
}

const unsetReaction = ""

// NoReaction is what an operator sets to stop godwit reacting to comments at all.
const NoReaction = "none"

// Receiver is the GitHub App webhook endpoint.
type Receiver struct {
	cfg     Config
	allowed map[string]bool
}

// New checks the receiver's configuration and returns it ready to serve; every fault here fails start-up.
func New(cfg Config) (*Receiver, error) {
	if cfg.Secret == "" {
		return nil, &configError{"the github webhook secret is required: an unverified endpoint is an open one"}
	}
	if cfg.Store == nil || cfg.API == nil || cfg.Log == nil {
		return nil, &configError{"the github receiver needs a store, an api client and a logger"}
	}
	if len(cfg.Associations) == 0 {
		cfg.Associations = defaultAssociations
	}
	allowed, err := parseAssociations(cfg.Associations)
	if err != nil {
		return nil, err
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = defaultMaxAge
	}
	if cfg.Runner == nil {
		cfg.Runner = recorder{log: cfg.Log}
	}
	if cfg.Reaction == unsetReaction {
		cfg.Reaction = defaultReaction
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Record == nil {
		cfg.Record = func(string, string) {}
	}

	return &Receiver{cfg: cfg, allowed: allowed}, nil
}

// Handler serves the App at /github/webhook and answers 404 everywhere else.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, r.serve)

	return mux
}

var knownEvents = []string{eventIssueComment, eventReview, eventPullRequest}

func eventLabel(event string) string {
	if slices.Contains(knownEvents, event) {
		return event
	}

	return "other"
}

func (r *Receiver) serve(w http.ResponseWriter, req *http.Request) {
	status, result, message := r.receive(w, req)
	r.cfg.Record(eventLabel(req.Header.Get(eventHeader)), result)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if message != "" {
		fmt.Fprintln(w, message)
	}
}

func (r *Receiver) receive(w http.ResponseWriter, req *http.Request) (int, string, string) {
	body, err := readBounded(w, req, r.cfg.MaxBodyBytes)
	if err != nil {
		var big *http.MaxBytesError
		if errors.As(err, &big) {
			return http.StatusRequestEntityTooLarge, resultOversize, ""
		}

		return http.StatusBadRequest, resultMalformed, ""
	}
	if !verify(r.cfg.Secret, req.Header.Get(signatureHeader), body) {
		return http.StatusUnauthorized, resultUnsigned, ""
	}
	if req.Method != http.MethodPost {
		return http.StatusMethodNotAllowed, resultMalformed, "godwit answers " + webhookPath + " on POST"
	}
	delivery := req.Header.Get(deliveryHeader)
	if delivery == "" {
		return http.StatusBadRequest, resultMalformed, "no " + deliveryHeader
	}

	return r.handle(req.Context(), req.Header.Get(eventHeader), delivery, body)
}

func readBounded(w http.ResponseWriter, req *http.Request, limit int) ([]byte, error) {
	defer func() { _ = req.Body.Close() }()

	return io.ReadAll(http.MaxBytesReader(w, req.Body, int64(limit)))
}

func (r *Receiver) handle(ctx context.Context, event, delivery string, body []byte) (int, string, string) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return http.StatusBadRequest, resultMalformed, "the delivery body is not a github payload"
	}
	cmd, out, err := r.accept(ctx, event, delivery, &p)
	switch {
	case err != nil:
		r.cfg.Log.Error("webhook delivery failed", "delivery", delivery, "event", event,
			"repository", p.Repository.FullName, "error", err)

		return http.StatusInternalServerError, resultError, "godwit could not answer this delivery"
	case out != nil:
		r.cfg.Log.Info("webhook delivery "+out.result, "delivery", delivery, "event", event,
			"repository", p.Repository.FullName, "detail", out.message)

		return http.StatusAccepted, out.result, out.message
	}

	return r.enqueue(ctx, delivery, event, *cmd)
}

func (r *Receiver) enqueue(ctx context.Context, delivery, event string, cmd command) (int, string, string) {
	duplicate := false
	err := r.cfg.Store.transact(ctx, func(tx txn) error {
		first, err := tx.RecordDelivery(ctx, delivery, event, cmd.repository)
		if err != nil {
			return err
		}
		if !first {
			duplicate = true

			return nil
		}

		return r.cfg.Runner.enqueue(ctx, tx, cmd)
	})
	switch {
	case err != nil:
		r.cfg.Log.Error("webhook enqueue failed", "delivery", delivery, "event", event, "error", err)

		return http.StatusInternalServerError, resultError, "godwit could not answer this delivery"
	case duplicate:
		return http.StatusAccepted, resultDuplicate, "delivery " + delivery + " was already handled"
	}

	return http.StatusAccepted, resultAccepted,
		fmt.Sprintf("godwit %s accepted at %s for %s", cmd.name, short(cmd.head), cmd.projectNames())
}

func (r *Receiver) decide(ctx context.Context, event, delivery string, p *payload) (*command, *outcome, *request, error) {
	req, out := parse(event, p)
	if out != nil {
		return nil, out, req, nil
	}
	if out := r.fresh(req); out != nil {
		return nil, out, req, nil
	}
	stored, err := r.cfg.Store.GitHubBindings(ctx)
	if err != nil {
		return nil, nil, req, err
	}
	bound := bind(stored, req.repository)
	if len(bound) == 0 {
		return nil, refused("repository %s is bound to no godwit target, so it gets nothing here — not an apply "+
			"and not a plan; ask a godwit operator to bind it (godwit target add <target> --github-repo %s)",
			req.repository, req.repository), req, nil
	}
	at, out, err := r.resolve(ctx, req, p)
	if out != nil || err != nil {
		return nil, out, req, err
	}
	res, err := resolve(ctx, at.repo, bound, req, at.head, at.files)
	if errors.Is(err, errTruncated) {
		return nil, tooLarge(req, err), req, nil
	}
	if err != nil {
		return nil, nil, req, err
	}
	if len(res.planned) == 0 {
		return nil, nothingToDo(req, res), req, nil
	}
	head := at.head

	return &command{
		delivery: delivery, event: event, repository: req.repository, installation: p.Installation.ID,
		number: req.number, head: head, login: req.commander,
		principal: api.Principal{Name: "github:" + req.repository, Scope: scopes[req.name]},
		bound:     bound, name: req.name, cmd: req.cmd, projects: res.planned,
		source: "github.com/" + req.repository + "@" + head,
	}, nil, req, nil
}

// refusedMarker is the Action's, so that if both ever ran on one pull request exactly one refusal stands.
const refusedMarker = "<!-- godwit:refused -->"

// tellable are the outcomes a person has to act on; an ignored delivery is not one.
var tellable = map[string]bool{resultRefused: true, resultStale: true}

func (r *Receiver) accept(ctx context.Context, event, delivery string, p *payload) (*command, *outcome, error) {
	cmd, out, req, err := r.decide(ctx, event, delivery, p)
	if out != nil && tellable[out.result] {
		r.tell(ctx, p, req, out)
	}

	return cmd, out, err
}

// checks is the mapping scripts/action-refuse.sh uses for the commit status the Action sets.
var checks = map[string]string{
	"plan": "godwit/plan", "apply": "godwit/applied", "confirm": "godwit/applied", "revert": "godwit/applied",
}

func (r *Receiver) tell(ctx context.Context, p *payload, req *request, out *outcome) {
	if req == nil || req.number <= 0 {
		return
	}
	repo, err := r.cfg.API.repository(ctx, p.Installation.ID, p.Repository.ID, req.repository)
	if err != nil {
		r.cfg.Log.Warn("could not answer on the pull request", "repository", req.repository,
			"pull_request", req.number, "error", err)

		return
	}
	if err := repo.speak(ctx, req.number, refusedMarker, refusal(req, out)); err != nil {
		r.cfg.Log.Warn("could not post the refusal on the pull request", "repository", req.repository,
			"pull_request", req.number, "error", err)
	}
	r.mark(ctx, repo, req, out)
}

// mark turns the check the command would have set red; a head godwit cannot read leaves the comment alone.
func (r *Receiver) mark(ctx context.Context, repo repoView, req *request, out *outcome) {
	name, ok := checks[req.name]
	if !ok {
		return
	}
	head := req.headSHA
	if !validSHA(head) {
		pr, err := repo.pullRequest(ctx, req.number)
		if err != nil || !validSHA(pr.head) {
			r.cfg.Log.Warn("could not set the refusal check", "repository", req.repository,
				"pull_request", req.number, "check", name, "error", err)

			return
		}
		head = pr.head
	}
	if err := repo.check(ctx, name, head, "godwit "+req.name+" refused", out.message); err != nil {
		r.cfg.Log.Warn("could not set the refusal check", "repository", req.repository,
			"pull_request", req.number, "check", name, "error", err)
	}
}

func refusal(req *request, out *outcome) string {
	return fmt.Sprintf("## godwit %s refused\n\n%s\n\nNothing ran.\n", req.name, out.message)
}

// tooLarge is what a partial listing gets instead of a wrong answer, and it is never silence.
func tooLarge(req *request, err error) *outcome {
	return refused("%s, so it cannot tell which projects this pull request touches and will not guess; "+
		"godwit %s is refused rather than reported as nothing to do. Split the pull request, or land the "+
		"migrations in one of their own", err, req.name)
}

// nothingToDo is silence for a pull request that touched no project, and a reason for a person who asked.
func nothingToDo(req *request, res resolution) *outcome {
	if req.commander == "" && len(res.skipped) == 0 {
		return ignored("no bound project of %s has a when_modified the changed files match", req.repository)
	}
	if len(res.skipped) == 0 {
		return refused("godwit %s: this pull request changes nothing any bound project of %s plans",
			req.name, req.repository)
	}

	return refused("%s", strings.Join(res.skipped, "; "))
}

// at is the commit a delivery resolved to, the view it resolved through, and what the pull request changes.
type at struct {
	head  string
	repo  repoView
	files int
}

func (r *Receiver) resolve(ctx context.Context, req *request, p *payload) (at, *outcome, error) {
	open := func(ctx context.Context) (repoView, error) {
		return r.cfg.API.repository(ctx, p.Installation.ID, p.Repository.ID, req.repository)
	}
	if req.commander != "" {
		return authorizer{
			open: open, allowed: r.allowed, reaction: r.cfg.Reaction, log: r.cfg.Log,
		}.authorize(ctx, req)
	}
	if req.headRepo != req.repository {
		return at{}, refused("pull request #%d has its head in %s, not %s: a fork's pull request is not planned "+
			"against the targets of %s", req.number, orNone(req.headRepo), req.repository, req.repository), nil
	}
	repo, err := open(ctx)
	if err != nil {
		return at{}, nil, err
	}

	return at{head: req.headSHA, repo: repo, files: req.files}, nil, nil
}

// fresh ages a delivery by GitHub's own timestamp inside a body GitHub signed, never by a committer date.
func (r *Receiver) fresh(req *request) *outcome {
	if req.at.IsZero() {
		return nil
	}
	age := r.cfg.Now().Sub(req.at)
	if age <= r.cfg.MaxAge {
		return nil
	}

	return &outcome{result: resultStale, message: fmt.Sprintf(
		"godwit %s was posted %s ago and this delivery is past the %s a command may be acted on within",
		req.name, age.Round(time.Second), r.cfg.MaxAge)}
}

func verify(secret, signature string, body []byte) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := signaturePrefix + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(signature), []byte(want))
}
