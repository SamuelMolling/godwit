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
	"github.com/SamuelMolling/godwit/internal/authz"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/limits"
)

// service is called in process, so the App carries a principal and every call site runs authz.Authorize itself in place of the interceptor it never passes.
type service interface {
	PlanRun(context.Context, *connect.Request[godwitv1.PlanRunRequest]) (*connect.Response[godwitv1.PlanRunResponse], error)
	CreateRun(context.Context, *connect.Request[godwitv1.CreateRunRequest]) (*connect.Response[godwitv1.CreateRunResponse], error)
	ConfirmRollout(context.Context, *connect.Request[godwitv1.ConfirmRolloutRequest]) (*connect.Response[godwitv1.ConfirmRolloutResponse], error)
	RevertRun(context.Context, *connect.Request[godwitv1.RevertRunRequest]) (*connect.Response[godwitv1.RevertRunResponse], error)
	GetRun(context.Context, *connect.Request[godwitv1.GetRunRequest]) (*connect.Response[godwitv1.GetRunResponse], error)
	GetPlan(context.Context, *connect.Request[godwitv1.GetPlanRequest]) (*connect.Response[godwitv1.GetPlanResponse], error)
}

type runStore interface {
	RecordGitHubRun(ctx context.Context, g controlplane.GitHubRun) error
	ClaimGitHubReports(ctx context.Context, lease time.Duration, limit int) ([]controlplane.GitHubRun, error)
	MarkGitHubReported(ctx context.Context, runID, state string) error
	GitHubRunsOf(ctx context.Context, repository string, pull int, target string) ([]controlplane.GitHubRun, error)
}

const (
	defaultWorkers  = 2
	defaultQueue    = 64
	defaultTimeout  = 15 * time.Minute
	defaultInterval = 5 * time.Second
	defaultLease    = time.Minute
	reportBatch     = 16
)

// WorkerConfig is what carrying a command out needs; NewWorker fills in what is left zero.
type WorkerConfig struct {
	API       forge
	Service   service
	Runs      runStore
	Limits    limits.Limits
	PublicURL string
	Workers   int
	Queue     int
	Timeout   time.Duration
	Interval  time.Duration
	Log       *slog.Logger
}

// Worker runs the commands the receiver accepted, off the delivery's own request.
type Worker struct {
	cfg    WorkerConfig
	jobs   chan command
	drain  chan struct{}
	teller chan struct{}
	wg     sync.WaitGroup
	cancel context.CancelFunc
	mu     sync.RWMutex
	closed bool
}

// NewWorker returns a started Worker; Stop drains what it has accepted.
func NewWorker(cfg WorkerConfig) *Worker {
	if cfg.Workers <= 0 {
		cfg.Workers = defaultWorkers
	}
	if cfg.Queue <= 0 {
		cfg.Queue = defaultQueue
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	cfg.Limits = cfg.Limits.WithDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		cfg: cfg, jobs: make(chan command, cfg.Queue),
		drain: make(chan struct{}), teller: make(chan struct{}), cancel: cancel,
	}
	for range cfg.Workers {
		w.wg.Add(1)
		go w.serve(ctx)
	}
	go w.tell(ctx)

	return w
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
	<-w.teller
}

func (w *Worker) serve(ctx context.Context) {
	defer w.wg.Done()
	for cmd := range w.jobs {
		w.carry(ctx, cmd)
	}
}

var (
	// errQueueFull rolls the transaction back with the delivery unrecorded, so GitHub's redelivery is a fresh attempt rather than a duplicate godwit drops.
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
	unsaid := w.say(ctx, repo, cmd, all, log)
	w.settle(ctx, repo, cmd, all, unsaid, log)
}

func (w *Worker) moved(ctx context.Context, repo repoView, cmd command, log *slog.Logger) (bool, error) {
	pr, err := repo.pullRequest(ctx, cmd.number)
	if err != nil {
		log.Error("could not read the pull request the command came from", "error", err)

		return false, err
	}
	if pr.Head == cmd.head {
		return false, nil
	}
	log.Info("the head moved after the command was accepted, so it is left to the delivery that moved it",
		"now", pr.Head)

	return true, nil
}

type done struct {
	project    project
	body       string
	title      string
	conclusion string
	url        string
	check      int64
	run        string
}

const (
	conclusionSuccess = "success"
	conclusionFailure = "failure"
	conclusionAction  = "action_required"
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
	switch cmd.name {
	case "confirm":
		return w.confirm(ctx, cmd, p)
	case "revert":
		return w.revert(ctx, cmd, p)
	}
	files, err := migrations(ctx, repo, p.path(), cmd.head, w.cfg.Limits)
	if err != nil {
		return refusedBy(cmd, p, err)
	}
	if len(files) == 0 {
		return done{
			project: p, conclusion: conclusionSuccess, title: "nothing to " + cmd.name,
			body: fmt.Sprintf("## godwit %s\n\n`%s` holds no migration at %s, so there is nothing to %s against `%s`.\n",
				cmd.name, p.path(), short(cmd.head), cmd.name, p.target),
		}
	}
	if cmd.name == "apply" {
		return w.apply(ctx, cmd, p, files)
	}

	return w.plan(ctx, cmd, p, files)
}

func (w *Worker) plan(ctx context.Context, cmd command, p project, files []*godwitv1.MigrationFile) done {
	if err := authz.Authorize(godwitv1connect.GodwitServicePlanRunProcedure, cmd.principal); err != nil {
		return refusedBy(cmd, p, err)
	}
	res, err := w.cfg.Service.PlanRun(authz.WithPrincipal(ctx, cmd.principal), connect.NewRequest(&godwitv1.PlanRunRequest{
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

func (w *Worker) settle(ctx context.Context, repo repoView, cmd command, all []done, unsaid map[string]error, log *slog.Logger) {
	name := checks[cmd.name]
	for _, d := range all {
		if d.run != "" {
			w.bind(ctx, cmd, d, log)

			continue
		}
		if d.check == 0 {
			continue
		}
		if err := repo.endCheck(ctx, d.check, checkRun{
			name: cmd.checkName(name, d.project), head: cmd.head, url: d.url,
			title: d.title, summary: onlyHere(d.body, unsaid[cmd.markerFor(d.project)]), conclusion: d.conclusion,
		}); err != nil {
			log.Warn("could not conclude the check", "target", d.project.target, "error", err)
		}
	}
}

func (w *Worker) bind(ctx context.Context, cmd command, d done, log *slog.Logger) {
	if err := w.cfg.Runs.RecordGitHubRun(ctx, controlplane.GitHubRun{
		RunID: d.run, Repository: cmd.repository, RepositoryID: cmd.repositoryID, Installation: cmd.installation,
		PullRequest: cmd.number, Head: cmd.head, Command: cmd.name, Target: d.project.target,
		Marker: cmd.markerFor(d.project), Format: d.project.format, CheckRun: d.check,
	}); err != nil {
		log.Error("the run was created but not bound to the pull request, which will not hear about it",
			"run", d.run, "target", d.project.target, "error", err)
	}
}

// reportMarker is the Action's, so that if both ever ran on one pull request exactly one report stands.
var reportMarker = map[string]string{
	"plan": "<!-- godwit:plan -->", "apply": "<!-- godwit:migrate -->",
	"confirm": "<!-- godwit:migrate -->", "revert": "<!-- godwit:migrate -->",
}

func (w *Worker) say(ctx context.Context, repo repoView, cmd command, all []done, log *slog.Logger) map[string]error {
	if _, ok := reportMarker[cmd.name]; !ok || len(all) == 0 {
		return nil
	}
	unsaid := map[string]error{}
	for _, group := range grouped(cmd, all) {
		if err := repo.speak(ctx, cmd.number, group.marker, strings.Join(group.bodies, "\n---\n")); err != nil {
			log.Warn("could not post the report on the pull request", "error", err)
			unsaid[group.marker] = err
		}
	}

	return unsaid
}

func onlyHere(body string, err error) string {
	if err == nil {
		return body
	}

	return body + "\n---\n\ngodwit could not post this on the pull request, so it stands only here: " +
		plainError(err) + "\n"
}

type sticky struct {
	marker string
	bodies []string
}

// grouped keeps one comment per marker: a run reports itself later under its own, and a shared one would delete the neighbouring project's report.
func grouped(cmd command, all []done) []sticky {
	var out []sticky
	at := map[string]int{}
	for _, d := range all {
		marker := cmd.markerFor(d.project)
		i, ok := at[marker]
		if !ok {
			i = len(out)
			at[marker] = i
			out = append(out, sticky{marker: marker})
		}
		out[i].bodies = append(out[i].bodies, d.body)
	}

	return out
}

func (c command) rollout(p project) string {
	if c.cmd != nil && c.cmd.Rollout != "" {
		return c.cmd.Rollout
	}

	return p.rollout
}

func (c command) sourceOf(p project) string {
	return c.source + ":" + p.path()
}

func (c command) checkName(name string, p project) string {
	if len(c.projects) < 2 {
		return name
	}

	return name + " (" + p.target + ")"
}

func (c command) markerFor(p project) string {
	marker := reportMarker[c.name]
	if len(c.projects) < 2 {
		return marker
	}

	return strings.TrimSuffix(marker, " -->") + ":" + p.target + " -->"
}
