package githubapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// service is the control-plane API called in the process that serves it: the App carries a principal
// rather than a token, so api.Authorize stands in for the interceptor that would have made the decision.
type service interface {
	PlanRun(context.Context, *connect.Request[godwitv1.PlanRunRequest]) (*connect.Response[godwitv1.PlanRunResponse], error)
}

const (
	defaultWorkers = 2
	defaultQueue   = 64
	defaultTimeout = 15 * time.Minute
)

// WorkerConfig is what carrying a command out needs; NewWorker fills in what is left zero.
type WorkerConfig struct {
	API     forge
	Service service
	Limits  api.Limits
	// PublicURL is the base a check's details_url is built on; empty links nothing.
	PublicURL string
	// Workers bounds how many commands run at once; each may build scratch databases, so it spends
	// the scratch server's budget alongside --max-concurrent-diffs rather than within it.
	Workers int
	Queue   int
	Timeout time.Duration
	Log     *slog.Logger
}

// Worker runs the commands the receiver accepted, off the delivery's own request.
type Worker struct {
	cfg    WorkerConfig
	jobs   chan command
	drain  chan struct{}
	wg     sync.WaitGroup
	cancel context.CancelFunc
	mu     sync.RWMutex
	closed bool
}

// NewWorker returns a started Worker; Stop drains what it has accepted.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.API == nil || cfg.Service == nil || cfg.Log == nil {
		return nil, &configError{"the github worker needs an api client, the godwit service and a logger"}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultWorkers
	}
	if cfg.Queue <= 0 {
		cfg.Queue = defaultQueue
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	cfg.Limits = cfg.Limits.WithDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{cfg: cfg, jobs: make(chan command, cfg.Queue), drain: make(chan struct{}), cancel: cancel}
	for range cfg.Workers {
		w.wg.Add(1)
		go w.serve(ctx)
	}

	return w, nil
}

// Stop closes the queue and waits for what is in flight, cancelling it when ctx gives up first.
func (w *Worker) Stop(ctx context.Context) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()

		return
	}
	w.closed = true
	close(w.jobs)
	w.mu.Unlock()
	go func() { w.wg.Wait(); close(w.drain) }()
	select {
	case <-w.drain:
	case <-ctx.Done():
	}
	w.cancel()
}

func (w *Worker) serve(ctx context.Context) {
	defer w.wg.Done()
	for cmd := range w.jobs {
		w.carry(ctx, cmd)
	}
}

var (
	// errQueueFull leaves the delivery unrecorded, so the transaction rolls back and GitHub's
	// redelivery is a fresh attempt rather than a duplicate godwit drops.
	errQueueFull = errors.New("the github app has more commands queued than it can hold; retry the delivery")
	errStopping  = errors.New("the github app is shutting down")
)

func (w *Worker) enqueue(ctx context.Context, tx txn, cmd command) error {
	if err := tx.Audit(ctx, controlplane.AuditEntry{
		Actor:  cmd.principal.Name,
		Action: controlplane.AuditWebhookCommand,
		Detail: cmd.detail(),
	}); err != nil {
		return err
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return errStopping
	}
	select {
	case w.jobs <- cmd:
		return nil
	default:
		return errQueueFull
	}
}

func (w *Worker) carry(ctx context.Context, cmd command) {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	log := w.cfg.Log.With("repository", cmd.repository, "command", cmd.name,
		"delivery", cmd.delivery, "pull_request", cmd.number, "head", cmd.head)
	repo, err := w.cfg.API.repository(ctx, cmd.installation, cmd.repositoryID, cmd.repository)
	if err != nil {
		log.Error("could not reach the repository the command came from", "error", err)

		return
	}
	moved, err := w.moved(ctx, repo, cmd, log)
	if err != nil || moved {
		return
	}
	all := w.act(ctx, repo, cmd, log)
	w.say(ctx, repo, cmd, all, log)
	w.settle(ctx, repo, cmd, all, log)
}

// moved abandons a command whose head the pull request has left: the push that moved it arrived as
// its own delivery and is being planned under that one.
func (w *Worker) moved(ctx context.Context, repo repoView, cmd command, log *slog.Logger) (bool, error) {
	pr, err := repo.pullRequest(ctx, cmd.number)
	if err != nil {
		log.Error("could not read the pull request the command came from", "error", err)

		return false, err
	}
	if pr.head == cmd.head {
		return false, nil
	}
	log.Info("the head moved after the command was accepted, so it is left to the delivery that moved it",
		"now", pr.head)

	return true, nil
}

type done struct {
	project    project
	body       string
	title      string
	conclusion string
	url        string
	check      int64
}

const (
	conclusionSuccess = "success"
	conclusionFailure = "failure"
	// conclusionAction is a plan that worked and still needs a person: a hazard nobody acknowledged.
	conclusionAction = "action_required"
)

func (w *Worker) act(ctx context.Context, repo repoView, cmd command, log *slog.Logger) []done {
	opened := make([]int64, len(cmd.projects))
	for i, p := range cmd.projects {
		opened[i] = w.begin(ctx, repo, cmd, p, log)
	}
	out := make([]done, 0, len(cmd.projects))
	for i, p := range cmd.projects {
		d := w.one(ctx, repo, cmd, p)
		d.check = opened[i]
		out = append(out, d)
	}

	return out
}

func (w *Worker) one(ctx context.Context, repo repoView, cmd command, p project) done {
	if err := cmd.bound.grant(cmd.repository, p.root, p.target); err != nil {
		return refusedBy(cmd, p, err)
	}
	files, err := migrations(ctx, repo, p.path(), cmd.head, w.cfg.Limits)
	if err != nil {
		return refusedBy(cmd, p, err)
	}
	if len(files) == 0 {
		return done{
			project: p, conclusion: conclusionSuccess, title: "nothing to plan",
			body: fmt.Sprintf("## godwit %s\n\n`%s` holds no migration at %s, so there is nothing to plan against `%s`.\n",
				cmd.name, p.path(), short(cmd.head), p.target),
		}
	}

	return w.plan(ctx, cmd, p, files)
}

func (w *Worker) plan(ctx context.Context, cmd command, p project, files []*godwitv1.MigrationFile) done {
	if err := api.Authorize(godwitv1connect.GodwitServicePlanRunProcedure, cmd.principal); err != nil {
		return refusedBy(cmd, p, err)
	}
	res, err := w.cfg.Service.PlanRun(api.WithPrincipal(ctx, cmd.principal), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: p.target, Files: files, Rollout: cmd.rollout(p), AllowOutOfOrder: p.outOfOrder,
		Persist: true, Source: cmd.sourceOf(p),
	}))
	if err != nil {
		return refusedBy(cmd, p, err)
	}

	return planned(cmd, p, res.Msg, w.cfg.PublicURL)
}

func refusedBy(cmd command, p project, err error) done {
	return done{
		project: p, conclusion: conclusionFailure, title: "godwit " + cmd.name + " refused",
		body: fmt.Sprintf("## godwit %s refused for `%s`\n\n%s\n\nNothing ran.\n",
			cmd.name, p.target, fenced(plainError(err))),
	}
}

func fenced(s string) string { return "```\n" + s + "\n```" }

// plainError drops connect's own prefix, which names a wire code a pull request reader has no use for.
func plainError(err error) string {
	msg := err.Error()
	var c *connect.Error
	if errors.As(err, &c) {
		msg = c.Message()
	}

	return strings.TrimSpace(msg)
}

func (w *Worker) begin(ctx context.Context, repo repoView, cmd command, p project, log *slog.Logger) int64 {
	name, ok := checks[cmd.name]
	if !ok {
		return 0
	}
	id, err := repo.startCheck(ctx, checkRun{
		name: cmd.checkName(name, p), head: cmd.head, title: "godwit " + cmd.name,
		summary: "godwit is reading the migrations of `" + p.path() + "` at " + short(cmd.head) + ".",
	})
	if err != nil {
		log.Warn("could not open the check", "target", p.target, "error", err)

		return 0
	}

	return id
}

func (w *Worker) settle(ctx context.Context, repo repoView, cmd command, all []done, log *slog.Logger) {
	name := checks[cmd.name]
	for _, d := range all {
		if d.check == 0 {
			continue
		}
		if err := repo.endCheck(ctx, d.check, checkRun{
			name: cmd.checkName(name, d.project), head: cmd.head, url: d.url,
			title: d.title, summary: d.body, conclusion: d.conclusion,
		}); err != nil {
			log.Warn("could not conclude the check", "target", d.project.target, "error", err)
		}
	}
}

// reportMarker is the Action's, so that if both ever ran on one pull request exactly one report stands.
var reportMarker = map[string]string{
	"plan": "<!-- godwit:plan -->", "apply": "<!-- godwit:migrate -->",
	"confirm": "<!-- godwit:migrate -->", "revert": "<!-- godwit:migrate -->",
}

func (w *Worker) say(ctx context.Context, repo repoView, cmd command, all []done, log *slog.Logger) {
	marker, ok := reportMarker[cmd.name]
	if !ok || len(all) == 0 {
		return
	}
	bodies := make([]string, 0, len(all))
	for _, d := range all {
		bodies = append(bodies, d.body)
	}
	if err := repo.speak(ctx, cmd.number, marker, strings.Join(bodies, "\n---\n")); err != nil {
		log.Warn("could not post the report on the pull request", "error", err)
	}
}

func (c command) rollout(p project) string {
	if c.cmd != nil && c.cmd.Rollout != "" {
		return c.cmd.Rollout
	}

	return p.rollout
}

// sourceOf is the provenance the Action writes, <host>/<owner>/<repo>@<sha>:<dir>, which is what the
// report reads a commit link out of.
func (c command) sourceOf(p project) string {
	return c.source + ":" + p.path()
}

// checkName stays the bare name while there is one project, because that is what a required check names.
func (c command) checkName(name string, p project) string {
	if len(c.projects) < 2 {
		return name
	}

	return name + " (" + p.target + ")"
}
