package githubapp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/comment"
	"github.com/SamuelMolling/godwit/internal/config"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

type fakeService struct {
	mu         sync.Mutex
	got        []*godwitv1.PlanRunRequest
	res        *godwitv1.PlanRunResponse
	err        error
	hold       chan struct{}
	created    []*godwitv1.CreateRunRequest
	createRes  *godwitv1.CreateRunResponse
	createErr  error
	confirmed  []string
	confirmErr error
	reverted   []*godwitv1.RevertRunRequest
	revertRes  *godwitv1.RevertRunResponse
	revertErr  error
	run        *godwitv1.Run
	applied    []*godwitv1.RunMigration
	runErr     error
	plan       *godwitv1.Plan
	planErr    error
}

func (s *fakeService) CreateRun(_ context.Context, req *connect.Request[godwitv1.CreateRunRequest]) (*connect.Response[godwitv1.CreateRunResponse], error) {
	s.mu.Lock()
	s.created = append(s.created, req.Msg)
	s.mu.Unlock()
	if s.createErr != nil {
		return nil, s.createErr
	}
	res := s.createRes
	if res == nil {
		res = &godwitv1.CreateRunResponse{RunId: "run-1", PlanId: "plan-1"}
	}

	return connect.NewResponse(res), nil
}

func (s *fakeService) ConfirmRollout(_ context.Context, req *connect.Request[godwitv1.ConfirmRolloutRequest]) (*connect.Response[godwitv1.ConfirmRolloutResponse], error) {
	s.mu.Lock()
	s.confirmed = append(s.confirmed, req.Msg.GetRunId())
	s.mu.Unlock()
	if s.confirmErr != nil {
		return nil, s.confirmErr
	}

	return connect.NewResponse(&godwitv1.ConfirmRolloutResponse{}), nil
}

func (s *fakeService) RevertRun(_ context.Context, req *connect.Request[godwitv1.RevertRunRequest]) (*connect.Response[godwitv1.RevertRunResponse], error) {
	s.mu.Lock()
	s.reverted = append(s.reverted, req.Msg)
	s.mu.Unlock()
	if s.revertErr != nil {
		return nil, s.revertErr
	}
	res := s.revertRes
	if res == nil {
		res = &godwitv1.RevertRunResponse{RunId: "revert-1", Reverts: req.Msg.GetRunId(), Target: req.Msg.GetTarget()}
	}

	return connect.NewResponse(res), nil
}

func (s *fakeService) GetRun(_ context.Context, req *connect.Request[godwitv1.GetRunRequest]) (*connect.Response[godwitv1.GetRunResponse], error) {
	if s.runErr != nil {
		return nil, s.runErr
	}
	run := s.run
	if run == nil {
		run = &godwitv1.Run{Id: req.Msg.GetRunId(), Target: "orders", State: godwitv1.RunState_RUN_STATE_SUCCEEDED}
	}

	return connect.NewResponse(&godwitv1.GetRunResponse{Run: run, Applied: s.applied}), nil
}

func (s *fakeService) GetPlan(_ context.Context, _ *connect.Request[godwitv1.GetPlanRequest]) (*connect.Response[godwitv1.GetPlanResponse], error) {
	if s.planErr != nil {
		return nil, s.planErr
	}

	return connect.NewResponse(&godwitv1.GetPlanResponse{Plan: s.plan}), nil
}

func (s *fakeService) PlanRun(ctx context.Context, req *connect.Request[godwitv1.PlanRunRequest]) (*connect.Response[godwitv1.PlanRunResponse], error) {
	s.mu.Lock()
	s.got = append(s.got, req.Msg)
	s.mu.Unlock()
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	res := s.res
	if res == nil {
		res = onePending()
	}

	return connect.NewResponse(res), nil
}

func (s *fakeService) requests() []*godwitv1.PlanRunRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.got
}

func onePending() *godwitv1.PlanRunResponse {
	return &godwitv1.PlanRunResponse{
		Target: "orders", Rollout: controlplane.RolloutDirect, Validated: true, PlanId: "plan-1", PlanKey: "key-1",
		Migrations: []*godwitv1.PlannedMigration{{
			Version: 20260101000000, Name: "add_orders", Checksum: "abc",
			Statements: []*godwitv1.PlannedStatement{{Sql: "CREATE TABLE orders ()"}},
		}},
	}
}

func withHazard() *godwitv1.PlanRunResponse {
	res := onePending()
	res.Migrations[0].Statements[0].Hazards = []*godwitv1.PlannedHazard{
		{Code: "H002", Detail: "drops a column", Recipe: "expand then contract"},
	}

	return res
}

type harness struct {
	worker *Worker
	repo   *fakeRepo
	api    *fakeAPI
	svc    *fakeService
	runs   *fakeRuns
}

func newHarness(t *testing.T, repo *fakeRepo, cfg WorkerConfig) *harness {
	t.Helper()
	h := &harness{repo: repo, api: &fakeAPI{repo: repo}, svc: &fakeService{}, runs: newRunStore()}
	cfg.API, cfg.Service, cfg.Runs, cfg.Log = h.api, h.svc, h.runs, testLog
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = "https://godwit.test"
	}
	w := NewWorker(cfg)
	t.Cleanup(func() { w.Stop(context.Background()) })
	h.worker = w

	return h
}

func planningRepo() *fakeRepo {
	r := &fakeRepo{
		perm: map[string]string{"alice": "write"},
		pr:   pull{head: testHead, headRepo: testRepo, state: "open"},
	}
	r.listing = map[string]contents{"db/migrations": listed("20260101000000_add_orders.up.sql")}
	r.blobs = map[string]string{"db/migrations/20260101000000_add_orders.up.sql": "CREATE TABLE orders ();"}

	return r
}

func planCommand() command {
	return command{
		delivery: "d1", event: eventIssueComment, repository: testRepo, repositoryID: 42, installation: 7,
		number: 3, head: testHead, login: "alice", name: "plan",
		cmd:       &comment.Command{Name: "plan"},
		principal: api.Principal{Name: "github:" + testRepo, Scope: scopes["plan"]},
		bound:     bindings{{target: "orders"}},
		projects:  []project{{dir: "db/migrations", target: "orders", format: config.PlanFormatSchema}},
		source:    "github.com/" + testRepo + "@" + testHead,
	}
}

func TestThePlanIsPostedAsTheReportTheCLIRenders(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.worker.carry(context.Background(), planCommand())

	if len(h.repo.notices) != 1 {
		t.Fatalf("comments = %v", h.repo.notices)
	}
	body := h.repo.notices[0]
	for _, want := range []string{
		"<!-- godwit:plan -->", "## godwit plan", "20260101000000_add_orders",
		"<!-- godwit-plan-id: plan-1 -->", "<!-- godwit-plan-verdict: 1 to apply -->",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment missing %q:\n%s", want, body)
		}
	}
	if len(h.repo.opened) != 1 || h.repo.opened[0].name != "godwit/plan" || h.repo.opened[0].head != testHead {
		t.Fatalf("opened = %+v", h.repo.opened)
	}
	if len(h.repo.ended) != 1 {
		t.Fatalf("ended = %+v", h.repo.ended)
	}
	end := h.repo.ended[0]
	if end.conclusion != conclusionSuccess || end.title != "1 to apply" {
		t.Fatalf("check = %+v", end)
	}
	if end.url != "https://godwit.test/ui/plans/plan-1" {
		t.Fatalf("details_url = %q", end.url)
	}
}

func TestThePlanRequestCarriesWhatTheProjectAndTheCommentAskedFor(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	h := newHarness(t, repo, WorkerConfig{})
	cmd := planCommand()
	cmd.cmd.Rollout = "expand-contract"
	cmd.projects[0].outOfOrder = true
	h.worker.carry(context.Background(), cmd)

	got := h.svc.requests()
	if len(got) != 1 {
		t.Fatalf("requests = %d", len(got))
	}
	req := got[0]
	switch {
	case req.Target != "orders":
		t.Fatalf("target = %q", req.Target)
	case req.Rollout != "expand-contract":
		t.Fatalf("rollout = %q", req.Rollout)
	case !req.AllowOutOfOrder:
		t.Fatal("allow_out_of_order = false")
	case !req.Persist:
		t.Fatal("persist = false: an apply could not bind to this plan")
	case req.Source != "github.com/"+testRepo+"@"+testHead+":db/migrations":
		t.Fatalf("source = %q", req.Source)
	case len(req.Files) != 1 || req.Files[0].Name != "20260101000000_add_orders.up.sql":
		t.Fatalf("files = %+v", req.Files)
	}
}

// A rollout the comment does not name falls back to the project's own, not to the service default.
func TestTheProjectsRolloutIsUsedWhenTheCommentNamesNone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	cmd := planCommand()
	cmd.projects[0].rollout = "expand-contract"
	h.worker.carry(context.Background(), cmd)

	if got := h.svc.requests()[0].Rollout; got != "expand-contract" {
		t.Fatalf("rollout = %q", got)
	}
}

// A hazard on what the apply would run is not a broken plan, and not a green light either.
func TestAnUnacknowledgedHazardLeavesTheCheckNeedingAPerson(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.res = withHazard()
	h.worker.carry(context.Background(), planCommand())

	end := h.repo.ended[0]
	if end.conclusion != conclusionAction {
		t.Fatalf("conclusion = %q", end.conclusion)
	}
	if !strings.Contains(end.title, "1 hazard to acknowledge") {
		t.Fatalf("title = %q", end.title)
	}
}

// The push that moved the head arrived as its own delivery; planning the old one would report a plan
// for a commit nobody is looking at.
func TestAHeadThatMovedBetweenAcceptAndRunIsLeftToTheDeliveryThatMovedIt(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.pr.head = testOther
	h := newHarness(t, repo, WorkerConfig{})
	h.worker.carry(context.Background(), planCommand())

	if len(repo.notices) != 0 || len(repo.opened) != 0 || len(h.svc.requests()) != 0 {
		t.Fatalf("acted on a stale head: comments=%v checks=%v plans=%d",
			repo.notices, repo.opened, len(h.svc.requests()))
	}
}

// The binding is re-read when the command runs, not only when it was accepted.
func TestATargetTheRepositoryIsNoLongerBoundToIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	cmd := planCommand()
	cmd.bound = bindings{{target: "billing"}}
	h.worker.carry(context.Background(), cmd)

	if len(h.svc.requests()) != 0 {
		t.Fatal("planned against a target the repository is not bound to")
	}
	if !strings.Contains(h.repo.notices[0], "is not bound to a target named \"orders\"") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
	if h.repo.ended[0].conclusion != conclusionFailure {
		t.Fatalf("conclusion = %q", h.repo.ended[0].conclusion)
	}
}

func TestAMigrationSetGodwitCouldNotReadWholeIsRefused(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.listing["db/migrations"] = contents{entries: listed("a.up.sql").entries, capped: true}
	h := newHarness(t, repo, WorkerConfig{})
	h.worker.carry(context.Background(), planCommand())

	if len(h.svc.requests()) != 0 {
		t.Fatal("planned from a listing it could not trust")
	}
	if !strings.Contains(repo.notices[0], "contents api") {
		t.Fatalf("comment = %s", repo.notices[0])
	}
}

func TestAFileOverTheLimitIsRefusedWithoutFetchingAnything(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	h := newHarness(t, repo, WorkerConfig{Limits: api.Limits{FileBytes: 4}})
	h.worker.carry(context.Background(), planCommand())

	if len(repo.fetched) != 0 {
		t.Fatalf("fetched %v", repo.fetched)
	}
	if !strings.Contains(repo.notices[0], "limit 4") {
		t.Fatalf("comment = %s", repo.notices[0])
	}
}

func TestADirectoryWithNoMigrationYetIsNotARefusal(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.listing = map[string]contents{}
	h := newHarness(t, repo, WorkerConfig{})
	h.worker.carry(context.Background(), planCommand())

	if len(h.svc.requests()) != 0 {
		t.Fatal("asked for a plan of nothing")
	}
	if h.repo.ended[0].conclusion != conclusionSuccess || h.repo.ended[0].title != "nothing to plan" {
		t.Fatalf("check = %+v", h.repo.ended[0])
	}
	if !strings.Contains(repo.notices[0], "holds no migration") {
		t.Fatalf("comment = %s", repo.notices[0])
	}
}

// The reader of a pull request has no use for a wire code.
func TestARefusedPlanCarriesTheServicesOwnWords(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.svc.err = connect.NewError(connect.CodeFailedPrecondition, errors.New("target orders has a run in flight"))
	h.worker.carry(context.Background(), planCommand())

	body := h.repo.notices[0]
	if !strings.Contains(body, "target orders has a run in flight") {
		t.Fatalf("comment = %s", body)
	}
	if strings.Contains(body, "failed_precondition") {
		t.Fatalf("comment carries the wire code: %s", body)
	}
}

// No webhook may reach a procedure its command's scope does not carry, interceptor or not.
func TestAPrincipalWithoutTheScopeNeverReachesTheService(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	cmd := planCommand()
	cmd.principal.Scope = ""
	h.worker.carry(context.Background(), cmd)

	if len(h.svc.requests()) != 0 {
		t.Fatal("reached the service with no scope")
	}
	if !strings.Contains(h.repo.notices[0], "requires scope read") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
}

func TestAPlanFormatTheProjectAskedForThatDoesNotExist(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	cmd := planCommand()
	cmd.projects[0].format = "prose"
	h.worker.carry(context.Background(), cmd)

	if !strings.Contains(h.repo.notices[0], "unknown plan format") {
		t.Fatalf("comment = %s", h.repo.notices[0])
	}
}

func TestSeveralProjectsGetOneCommentAndACheckEach(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.listing["billing/db"] = listed("20260101000000_add_bills.up.sql")
	repo.blobs["billing/db/20260101000000_add_bills.up.sql"] = "CREATE TABLE bills ();"
	h := newHarness(t, repo, WorkerConfig{})
	cmd := planCommand()
	cmd.bound = bindings{{target: "orders"}, {target: "billing", dir: "billing"}}
	cmd.projects = append(cmd.projects, project{
		root: "billing", dir: "db", target: "billing", format: config.PlanFormatSchema,
	})
	h.worker.carry(context.Background(), cmd)

	// One sticky comment per project, not one carrying both: a run reports itself later under its own
	// marker, and a shared one would delete the neighbouring project's report.
	if len(repo.notices) != 2 {
		t.Fatalf("comments = %d, want one per project", len(repo.notices))
	}
	for i, want := range []string{"<!-- godwit:plan:orders -->", "<!-- godwit:plan:billing -->"} {
		if !strings.HasPrefix(repo.notices[i], want) {
			t.Fatalf("comment %d = %s, want marker %s", i, repo.notices[i], want)
		}
	}
	names := []string{repo.opened[0].name, repo.opened[1].name}
	if names[0] != "godwit/plan (orders)" || names[1] != "godwit/plan (billing)" {
		t.Fatalf("check names = %v", names)
	}
	if len(repo.ended) != 2 {
		t.Fatalf("ended = %d", len(repo.ended))
	}
}

func TestTheWorkStillHappensWhenGitHubWillNotTakeTheCheckOrTheComment(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.openErr, repo.noticeErr, repo.endErr = errBroken, errBroken, errBroken
	h := newHarness(t, repo, WorkerConfig{})
	h.worker.carry(context.Background(), planCommand())

	if len(h.svc.requests()) != 1 {
		t.Fatalf("plans = %d", len(h.svc.requests()))
	}
	if len(repo.ended) != 0 {
		t.Fatalf("concluded a check that never opened: %+v", repo.ended)
	}
}

func TestACommandGodwitCannotOpenTheRepositoryFor(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.api.err = errBroken
	h.worker.carry(context.Background(), planCommand())

	if len(h.svc.requests()) != 0 {
		t.Fatal("planned without reading the repository")
	}
}

func TestACommandGodwitCannotReadThePullRequestFor(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.pullErr = errBroken
	h := newHarness(t, repo, WorkerConfig{})
	h.worker.carry(context.Background(), planCommand())

	if len(repo.opened) != 0 || len(h.svc.requests()) != 0 {
		t.Fatal("acted on a head it could not confirm")
	}
}

func TestACommandWithNoCheckOfItsOwnStillReports(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	cmd := planCommand()
	cmd.name = "verify"
	h.worker.carry(context.Background(), cmd)

	if len(h.repo.opened) != 0 || len(h.repo.ended) != 0 {
		t.Fatalf("opened a check for a command that sets none: %+v", h.repo.opened)
	}
	if len(h.repo.notices) != 0 {
		t.Fatalf("commented for a command with no report: %v", h.repo.notices)
	}
}

func TestAnAcceptedCommandIsAuditedAndQueued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	store := newStore(nil)
	if err := h.worker.enqueue(context.Background(), store, planCommand()); err != nil {
		t.Fatal(err)
	}
	if len(store.audits) != 1 || store.audits[0].Action != controlplane.AuditWebhookCommand {
		t.Fatalf("audits = %+v", store.audits)
	}
	if store.audits[0].Actor != "github:"+testRepo {
		t.Fatalf("actor = %q", store.audits[0].Actor)
	}
}

func TestACommandTheAuditCouldNotRecordIsNotQueued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	store := newStore(nil)
	store.auditErr = errBroken
	if err := h.worker.enqueue(context.Background(), store, planCommand()); !errors.Is(err, errBroken) {
		t.Fatalf("err = %v", err)
	}
	if len(h.worker.jobs) != 0 {
		t.Fatal("queued a command the audit refused")
	}
}

// A full queue must fail the delivery rather than drop it: the transaction rolls back, the delivery id
// is not recorded, and GitHub's redelivery is a fresh attempt.
func TestAFullQueueFailsTheDeliveryRatherThanDroppingIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{Queue: 1, Workers: 1})
	h.svc.hold = make(chan struct{})
	t.Cleanup(func() { close(h.svc.hold) })
	store := newStore(nil)
	var full error
	for range 8 {
		if err := h.worker.enqueue(context.Background(), store, planCommand()); err != nil {
			full = err

			break
		}
	}
	if !errors.Is(full, errQueueFull) {
		t.Fatalf("err = %v", full)
	}
}

func TestACommandArrivingAsTheAppStopsIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{})
	h.worker.Stop(context.Background())
	h.worker.Stop(context.Background())
	if err := h.worker.enqueue(context.Background(), newStore(nil), planCommand()); !errors.Is(err, errStopping) {
		t.Fatalf("err = %v", err)
	}
}

func TestStopCarriesOutWhatItAlreadyAccepted(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{Workers: 1})
	if err := h.worker.enqueue(context.Background(), newStore(nil), planCommand()); err != nil {
		t.Fatal(err)
	}
	h.worker.Stop(context.Background())
	if len(h.svc.requests()) != 1 {
		t.Fatalf("plans = %d: a command godwit accepted was dropped", len(h.svc.requests()))
	}
}

func TestStopGivesUpWhenItsDeadlineDoes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, planningRepo(), WorkerConfig{Workers: 1})
	h.svc.hold = make(chan struct{})
	if err := h.worker.enqueue(context.Background(), newStore(nil), planCommand()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	h.worker.Stop(ctx)
	close(h.svc.hold)
}

func TestAWorkerNeedsWhatItCannotRunWithout(t *testing.T) {
	t.Parallel()

	w := NewWorker(WorkerConfig{API: &fakeAPI{}, Service: &fakeService{}, Runs: newRunStore(), Log: testLog})
	defer w.Stop(context.Background())
	if w.cfg.Workers != defaultWorkers || w.cfg.Queue != defaultQueue || w.cfg.Timeout != defaultTimeout {
		t.Fatalf("defaults = %+v", w.cfg)
	}
	if w.cfg.Limits.FileBytes != api.DefaultFileBytes {
		t.Fatalf("limits = %+v", w.cfg.Limits)
	}
}

func TestACheckGodwitCannotConclude(t *testing.T) {
	t.Parallel()

	repo := planningRepo()
	repo.endErr = errBroken
	h := newHarness(t, repo, WorkerConfig{})
	h.worker.carry(context.Background(), planCommand())

	if len(repo.notices) != 1 {
		t.Fatalf("the report was not posted: %v", repo.notices)
	}
}
