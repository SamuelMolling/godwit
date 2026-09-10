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
	"time"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// Path is the only path the webhook listener serves.
const Path = "/github/webhook"

const (
	signatureHeader = "X-Hub-Signature-256"
	deliveryHeader  = "X-GitHub-Delivery"
	eventHeader     = "X-GitHub-Event"
	signaturePrefix = "sha256="
)

// DefaultMaxBodyBytes bounds what one delivery may make this process buffer before it is verified.
const DefaultMaxBodyBytes = 1 << 20

// DefaultMaxAge is how old a command's own timestamp may be before its delivery is refused.
const DefaultMaxAge = time.Hour

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

// Tx is the store inside the transaction that records a delivery and enqueues whatever it asked for.
type Tx interface {
	RecordDelivery(ctx context.Context, id, event, repository string) (bool, error)
	Audit(ctx context.Context, e controlplane.AuditEntry) error
}

// Store is the receiver's view of the control plane.
type Store interface {
	GitHubBindings(ctx context.Context) (map[string]string, error)
	Transact(ctx context.Context, fn func(Tx) error) error
}

type store struct{ *controlplane.Store }

// Transact narrows the control plane's own transaction to the two writes a receipt makes.
func (s store) Transact(ctx context.Context, fn func(Tx) error) error {
	return s.Store.Transact(ctx, func(tx *controlplane.Store) error { return fn(tx) })
}

// Adapt returns the receiver's view of the control-plane store.
func Adapt(s *controlplane.Store) Store { return store{s} }

// Config is everything the receiver needs to answer a delivery.
type Config struct {
	Secret       string
	MaxBodyBytes int
	MaxAge       time.Duration
	Associations []string
	Store        Store
	API          API
	Runner       Runner
	Record       func(event, result string)
	Log          *slog.Logger
	Now          func() time.Time
}

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
		cfg.Associations = DefaultAssociations
	}
	allowed, err := parseAssociations(cfg.Associations)
	if err != nil {
		return nil, err
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if cfg.Runner == nil {
		cfg.Runner = Recorder{Log: cfg.Log}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Record == nil {
		cfg.Record = func(string, string) {}
	}

	return &Receiver{cfg: cfg, allowed: allowed}, nil
}

// Handler serves the receiver at Path and nothing else.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(Path, r)

	return mux
}

var knownEvents = []string{EventIssueComment, EventReview, EventPullRequest}

func eventLabel(event string) string {
	if slices.Contains(knownEvents, event) {
		return event
	}

	return "other"
}

// ServeHTTP answers one delivery; a request that fails the signature reaches no JSON, no store and no body.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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
		return http.StatusMethodNotAllowed, resultMalformed, "godwit answers " + Path + " on POST"
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
	cmd, out, err := r.command(ctx, event, delivery, &p)
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

func (r *Receiver) enqueue(ctx context.Context, delivery, event string, cmd Command) (int, string, string) {
	duplicate := false
	err := r.cfg.Store.Transact(ctx, func(tx Tx) error {
		first, err := tx.RecordDelivery(ctx, delivery, event, cmd.Repository)
		if err != nil {
			return err
		}
		if !first {
			duplicate = true

			return nil
		}

		return r.cfg.Runner.Enqueue(ctx, tx, cmd)
	})
	switch {
	case err != nil:
		r.cfg.Log.Error("webhook enqueue failed", "delivery", delivery, "event", event, "error", err)

		return http.StatusInternalServerError, resultError, "godwit could not answer this delivery"
	case duplicate:
		return http.StatusAccepted, resultDuplicate, "delivery " + delivery + " was already handled"
	}

	return http.StatusAccepted, resultAccepted, "godwit " + cmd.Name + " accepted at " + short(cmd.Head)
}

func (r *Receiver) command(ctx context.Context, event, delivery string, p *payload) (*Command, *outcome, error) {
	req, out := parse(event, p)
	if out != nil {
		return nil, out, nil
	}
	if out := r.fresh(req); out != nil {
		return nil, out, nil
	}
	stored, err := r.cfg.Store.GitHubBindings(ctx)
	if err != nil {
		return nil, nil, err
	}
	bindings := bind(stored, req.repository)
	if len(bindings) == 0 {
		return nil, refused("repository %s is bound to no godwit target, so it gets nothing here — not an apply "+
			"and not a plan; ask a godwit operator to bind it (godwit target add <target> --github-repo %s)",
			req.repository, req.repository), nil
	}
	head, out, err := r.resolve(ctx, req, p)
	if out != nil || err != nil {
		return nil, out, err
	}

	return &Command{
		Delivery: delivery, Event: event, Repository: req.repository, Installation: p.Installation.ID,
		Number: req.number, Head: head, Login: req.commander,
		Principal: api.Principal{Name: "github:" + req.repository, Scope: scopes[req.name]},
		Bindings:  bindings, Name: req.name, Comment: req.cmd,
		Source: "github.com/" + req.repository + "@" + head,
	}, nil, nil
}

// resolve leaves a pull request event with no commander, so a plan spends no installation budget.
func (r *Receiver) resolve(ctx context.Context, req *request, p *payload) (string, *outcome, error) {
	if req.commander == "" {
		if req.headRepo != req.repository {
			return "", refused("pull request #%d has its head in %s, not %s: a fork's pull request is not planned "+
				"against the targets of %s", req.number, orNone(req.headRepo), req.repository, req.repository), nil
		}

		return req.headSHA, nil, nil
	}
	open := func(ctx context.Context) (Repo, error) {
		return r.cfg.API.Repository(ctx, p.Installation.ID, p.Repository.ID, req.repository)
	}

	return authorizer{open: open, allowed: r.allowed}.authorize(ctx, req)
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
