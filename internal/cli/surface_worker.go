package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// A worker launch (`surface launch --worktree <branch>`) starts one agent for
// a coordinator: its own git worktree, its own herdr workspace, and a ledger
// row that says what was created at each step.

// maxLedgerFailureLen bounds the error text kept in a failed row.
const maxLedgerFailureLen = 240

// workerLedger is the slice of *worker.Ledger a launch writes.
type workerLedger interface {
	Begin(worker.Row) error
	Update(name string, fn func(*worker.Row)) error
}

// workerSteps are the three things a worker launch does, as functions so the
// ledger bookkeeping between them can be tested without git or herdr.
type workerSteps struct {
	// identify reads the repository identity the row records, before the
	// row is written; a drain launch also refuses a branch that exists.
	identify    func(ctx context.Context) (repoIdentity, error)
	addWorktree func(ctx context.Context) (worker.Worktree, error)
	build       func(cwd string) (launch.BuiltInvocation, error)
	launch      func(ctx context.Context, inv launch.Invocation) (backend.Ref, error)
	now         func() time.Time
	// brief is the first brief's ledger record, or nil for a launch with
	// none. It is written with the pending row, before anything starts.
	brief *worker.Brief
	// launchID is the queue claim the launch is for, written with the
	// pending row; empty for a CLI launch.
	launchID string
}

// workerLaunched is what a successful worker launch reports.
type workerLaunched struct {
	ref      backend.Ref
	worktree string
}

// runWorkerSteps writes a pending row before anything is created, then moves
// the row forward after each step. A failure at any step leaves the row in
// StageFailed with whatever the earlier steps recorded, so the next
// coordinator can find a worktree or workspace a dead launch left behind.
func runWorkerSteps(ctx context.Context, led workerLedger, name, branch string, s workerSteps) (workerLaunched, error) {
	if s.identify == nil {
		return workerLaunched{}, errNoIdentityStep
	}
	// Before Begin: a failed read, or a drain branch that already exists,
	// leaves no row and nothing else.
	id, err := s.identify(ctx)
	if err != nil {
		return workerLaunched{}, err
	}
	if err := led.Begin(worker.Row{Name: name, Branch: branch, StartedAt: s.now().UTC(), Brief: s.brief, LaunchID: s.launchID,
		GitHubRepo: id.NameWithOwner, GitHubRepoID: id.DatabaseID}); err != nil {
		return workerLaunched{}, err
	}
	// created is a worktree path the attempt made, recorded on failure so the
	// row never reads as having created nothing.
	var created string
	fail := func(err error) (workerLaunched, error) {
		var recovery string
		var launchErr *surface.LaunchError
		if errors.As(err, &launchErr) {
			recovery = launchErr.Recovery
		}
		if uerr := led.Update(name, func(r *worker.Row) {
			r.Stage = worker.StageFailed
			r.Failure = termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
			r.Recovery = recovery
			if created != "" && r.Worktree == "" {
				r.Worktree = created
			}
		}); uerr != nil {
			return workerLaunched{}, errors.Join(err, fmt.Errorf("record the failure in the worker ledger: %w", uerr))
		}
		return workerLaunched{}, err
	}

	wt, err := s.addWorktree(ctx)
	if err != nil {
		created = wt.Path
		return fail(err)
	}
	if err := led.Update(name, func(r *worker.Row) {
		r.Stage = worker.StageWorktree
		r.Worktree = wt.Path
		r.Base = wt.Base
		r.BranchFrom = wt.BranchFrom
	}); err != nil {
		return fail(err)
	}

	built, err := s.build(wt.Path)
	if err != nil {
		return fail(launchConfigError{err})
	}
	// A worker must be built as one: that is what applies the posture floor,
	// the worker settings and the environment allowlist. A build step that
	// skipped it would launch a worker with the launcher's posture and env.
	if !built.Worker {
		return fail(launchConfigError{errors.New("forgectl: the worker invocation was not built as a worker launch")})
	}
	if err := led.Update(name, func(r *worker.Row) {
		r.Harness = built.Invocation.Harness
		if built.SessionID != "" {
			r.SessionID = built.SessionID
			r.Transcript = worker.TranscriptPath(built.Invocation.Env, built.Invocation.CWD, built.SessionID)
		}
	}); err != nil {
		return fail(err)
	}

	ref, err := s.launch(ctx, built.Invocation)
	if err != nil {
		return fail(err)
	}
	// From here the worker is running, so a failure is bookkeeping only and
	// the live ref goes back with it; recording StageFailed would orphan the
	// workspace.
	encoded, err := ref.MarshalJSON()
	if err != nil {
		return workerLaunched{ref: ref, worktree: wt.Path},
			fmt.Errorf("the worker started but its surface reference could not be recorded: %w", err)
	}
	if err := led.Update(name, func(r *worker.Row) {
		r.Stage = worker.StageLaunched
		r.Ref = encoded
	}); err != nil {
		// The worker is running; only the bookkeeping failed. Say so rather
		// than report a launch failure the operator would retry.
		return workerLaunched{ref: ref, worktree: wt.Path},
			fmt.Errorf("the worker started but its ledger row could not be completed: %w", err)
	}
	return workerLaunched{ref: ref, worktree: wt.Path}, nil
}

// errLaunchConfig marks a launch that failed because its configuration
// cannot build a worker: the build step (posture, harness profile, binary
// resolution) or a build that is not a worker. It fails the same way for
// every worker, so the drain pauses on it rather than failing row after row.
var errLaunchConfig = errors.New("forgectl: the launch configuration cannot build a worker")

// launchConfigError wraps a build-step error as errLaunchConfig without
// changing its text, which the CLI prints and the ledger records.
type launchConfigError struct{ err error }

func (e launchConfigError) Error() string        { return e.err.Error() }
func (e launchConfigError) Unwrap() error        { return e.err }
func (e launchConfigError) Is(target error) bool { return target == errLaunchConfig }

// sessionNamer is implemented by the herdr adapter.
type sessionNamer interface{ Session() string }

// workerSpec is one worker launch: the CLI builds it from its flags, and an
// in-process caller (the drain) builds it directly.
type workerSpec struct {
	target    string // repo, as a project name or path
	name      string
	branch    string
	harness   string
	allowPATH bool
	// launchID is the drain's queue claim; empty for the CLI.
	launchID string
	// configDir is the resolved [surface.profiles] config_dir, the worker's
	// CLAUDE_CONFIG_DIR, or empty for the launcher's own.
	configDir string
	// model replaces the launch profile's model, or is empty.
	model string
	// drain marks the drain's launch: its branch must not exist yet, in the
	// checkout or on GitHub (worker.ErrBranchExists).
	drain bool
}

// request is the build request for this worker at cwd: the one a surface
// launch builds, plus the worker's profile config dir and model.
func (spec workerSpec) request(cfg config.LaunchConfig, cwd string, injected map[string]string, unset []string) launch.InvocationRequest {
	req := surfaceInvocationRequest(cfg, cwd, injected, unset, spec.harness)
	req.ConfigDir, req.Model = spec.configDir, spec.model
	return req
}

// workerSetup is what a worker launch resolves before it writes anything.
type workerSetup struct {
	adapter  backend.Adapter
	top      string
	led      *worker.Ledger
	self     string
	injected map[string]string
	unset    []string
}

// prepareWorker validates the name and resolves the herdr adapter, the repo
// top, its ledger, and the injected environment. warn takes the adapter's
// setup warnings.
func prepareWorker(ctx context.Context, warn io.Writer, deps module.Deps, spec workerSpec) (workerSetup, error) {
	if err := worker.ValidName(spec.name); err != nil {
		return workerSetup{}, WithExitCode(fmt.Errorf("--name: %w", err), exitUsage)
	}

	adapter, err := newHerdrAdapter(warn)
	if err != nil {
		return workerSetup{}, WithExitCode(err, exitUsage)
	}
	namer, ok := adapter.(sessionNamer)
	if !ok {
		return workerSetup{}, errors.New("forgectl: the herdr adapter does not report its session")
	}

	target, err := projects.New(deps.Runner).ResolveTarget(spec.target)
	if err != nil {
		return workerSetup{}, WithExitCode(err, exitUsage)
	}
	top, err := worker.RepoTop(ctx, deps.Runner, target)
	if err != nil {
		return workerSetup{}, WithExitCode(err, exitUsage)
	}
	led, err := worker.Open(top, namer.Session())
	if err != nil {
		return workerSetup{}, err
	}

	self, err := surface.SelfPath()
	if err != nil {
		return workerSetup{}, err
	}
	injected, unset, err := injectedLaunchEnv(deps.Cfg)
	if err != nil {
		return workerSetup{}, WithExitCode(termsafe.Error(err), exitUsage)
	}
	return workerSetup{adapter: adapter, top: top, led: led, self: self, injected: injected, unset: unset}, nil
}

// steps wires the real launch steps. The build step always goes through
// buildWorkerInvocation, which applies the worker floor and the environment
// allowlist; runWorkerSteps refuses a build that was not made as a worker.
// warn takes base-lookup and build notes.
func (s workerSetup) steps(deps module.Deps, spec workerSpec, prompt string, brief *worker.Brief, warn io.Writer) workerSteps {
	service := surface.NewService(s.adapter, surface.Policy{AllowPATHBinary: spec.allowPATH}, "")
	return workerSteps{
		identify: func(ctx context.Context) (repoIdentity, error) {
			return workerRepoIdentity(ctx, deps.Runner, warn, s.top, spec.branch, spec.drain)
		},
		addWorktree: func(ctx context.Context) (worker.Worktree, error) {
			return worker.AddWorktree(ctx, deps.Runner, s.top, spec.name, spec.branch, func() (string, error) {
				return workerBase(ctx, deps.Runner, s.top, warn)
			})
		},
		build: func(cwd string) (launch.BuiltInvocation, error) {
			return buildWorkerInvocation(spec.request(deps.Cfg.Launch, cwd, s.injected, s.unset), prompt, worker.NewSessionID, warn)
		},
		launch: func(ctx context.Context, inv launch.Invocation) (backend.Ref, error) {
			inv.Env = markHerdrPane(inv.Env)
			req := surface.NewLaunchRequest(spec.name, inv)
			req.Self = s.self
			result, err := service.Launch(ctx, req)
			if err != nil {
				return backend.Ref{}, err
			}
			return result.Ref(), nil
		},
		now:      time.Now,
		brief:    brief,
		launchID: spec.launchID,
	}
}

// workerAttempt is what one launch attempt left behind.
type workerAttempt struct {
	launched workerLaunched
	// row is the ledger row the attempt wrote, read back once it ended. It is
	// nil when the attempt wrote no row: it failed before or at Begin (a
	// taken name is refused there, and that row is not this attempt's).
	row *worker.Row
	// rowErr is set when the attempt wrote a row that could not be read back.
	rowErr error
}

// createdNothing reports whether the attempt is known to have created
// nothing: it wrote no row, or its row is a failed one naming no worktree,
// workspace, or recovery tag (worker.CreatedNothing). A row that could not be
// read back is not known to be empty.
func (a workerAttempt) createdNothing() bool {
	if a.rowErr != nil {
		return false
	}
	return a.row == nil || worker.CreatedNothing(*a.row)
}

// beginWatch records whether Begin wrote the attempt's row.
type beginWatch struct {
	workerLedger
	began bool
}

func (b *beginWatch) Begin(r worker.Row) error {
	err := b.workerLedger.Begin(r)
	b.began = err == nil
	return err
}

// attemptWorker runs the steps and reads back the row the attempt left, so a
// caller can tell a failure before anything was created from one after.
func attemptWorker(ctx context.Context, led *worker.Ledger, name, branch string, s workerSteps) (workerAttempt, error) {
	watch := &beginWatch{workerLedger: led}
	launched, err := runWorkerSteps(ctx, watch, name, branch, s)
	attempt := workerAttempt{launched: launched}
	if !watch.began {
		return attempt, err
	}
	rows, rerr := led.Rows()
	if rerr != nil {
		attempt.rowErr = rerr
		return attempt, err
	}
	if r, ok := findRow(rows, name); ok {
		attempt.row = &r
	} else {
		attempt.rowErr = fmt.Errorf("worker %q: the row this launch wrote is no longer in the ledger", name)
	}
	return attempt, err
}

// launchWorker is the in-process worker launch, for a caller with no cobra
// command: the brief arrives as text, not as a --brief argument, and is
// checked with worker.CheckBrief. warn takes the launch's setup warnings and
// notes. It returns the attempt alongside any error.
func launchWorker(ctx context.Context, warn io.Writer, deps module.Deps, spec workerSpec, briefText string) (workerAttempt, error) {
	setup, err := prepareWorker(ctx, warn, deps, spec)
	if err != nil {
		return workerAttempt{}, err
	}
	prompt, brief, err := composeLaunchBrief(briefText, time.Now)
	if err != nil {
		return workerAttempt{}, err
	}
	return attemptWorker(ctx, setup.led, spec.name, spec.branch, setup.steps(deps, spec, prompt, brief, warn))
}

// runWorkerLaunch wires the real steps: herdr only, a validated name, the repo
// top as the ledger key, and the worker posture floor on the invocation.
func runWorkerLaunch(cmd *cobra.Command, deps module.Deps, opts surfaceLaunchOptions) error {
	if opts.Backend != "herdr" {
		return WithExitCode(errors.New("--worktree needs --surface herdr; workers run in herdr only"), exitUsage)
	}
	if opts.Model != "" {
		if err := config.CheckModelName(opts.Model); err != nil {
			return WithExitCode(fmt.Errorf("--model: %w", err), exitUsage)
		}
	}
	configDir, err := deps.Cfg.Surface.ProfileConfigDir(opts.Profile, os.UserHomeDir)
	if err != nil {
		return WithExitCode(termsafe.Error(fmt.Errorf("--profile: %w", err)), exitUsage)
	}
	if configDir != "" && opts.Harness != "" && opts.Harness != "claude" {
		return WithExitCode(errProfileNotClaude, exitUsage)
	}
	spec := workerSpec{target: opts.Target, name: opts.DisplayName, branch: opts.Worktree, harness: opts.Harness, allowPATH: opts.AllowPATH,
		configDir: configDir, model: opts.Model}
	ctx, warn := cmd.Context(), cmd.ErrOrStderr()
	setup, err := prepareWorker(ctx, warn, deps, spec)
	if err != nil {
		return err
	}
	if err := checkProfileHarness(deps.Cfg.Launch, worker.WorktreePath(setup.top, spec.name), spec); err != nil {
		return WithExitCode(err, exitUsage)
	}
	prompt, brief, err := launchBrief(opts.Brief, time.Now)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	if opts.DryRun {
		return planWorkerLaunch(cmd, deps, opts, setup.top, setup.led, workerPlanInputs{spec: spec, injected: setup.injected, unset: setup.unset, prompt: prompt, hasBrief: brief != nil, self: setup.self})
	}

	attempt, err := attemptWorker(ctx, setup.led, spec.name, spec.branch, setup.steps(deps, spec, prompt, brief, warn))
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(out, safeTitle(attempt.launched.ref.String())); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, safePath(attempt.launched.worktree))
	return err
}

// errProfileNotClaude refuses --profile for a worker that is not claude.
var errProfileNotClaude = errors.New("--profile sets CLAUDE_CONFIG_DIR and applies to claude workers only")

// checkProfileHarness refuses a --profile for a worker whose harness, after
// the --harness override or else the launch profile matched at its worktree,
// is not claude. It runs before the worktree exists; the build step would
// refuse the same launch only after it.
func checkProfileHarness(lc config.LaunchConfig, worktree string, spec workerSpec) error {
	if spec.configDir == "" {
		return nil
	}
	harness := spec.harness
	if harness == "" {
		p, err := launch.Resolve(lc, worktree)
		if err != nil {
			return err
		}
		harness = p.Harness
	}
	if harness != "claude" {
		return errProfileNotClaude
	}
	return nil
}

// buildWorkerInvocation marks req as a worker launch, which is what turns on
// the worker floor, the worker settings and the environment allowlist
// (launch.BuildInvocation), and gives a claude worker its session id. The
// harness is known only once the profile resolves, so a claude worker is
// built a second time with the id.
func buildWorkerInvocation(req launch.InvocationRequest, prompt string, newID func() (string, error), warn io.Writer) (launch.BuiltInvocation, error) {
	req.Worker = true
	req.Prompt = prompt
	built, err := launch.BuildInvocation(req)
	if err == nil && built.Invocation.Harness == "claude" {
		if req.SessionID, err = newID(); err != nil {
			return launch.BuiltInvocation{}, err
		}
		built, err = launch.BuildInvocation(req)
	}
	if err != nil {
		return built, err
	}
	for _, n := range built.Notes {
		if _, werr := fmt.Fprintln(warn, "forgectl: note: "+termsafe.SafeLineMax(n, 300)); werr != nil {
			return launch.BuiltInvocation{}, werr
		}
	}
	return built, nil
}

// launchBrief turns --brief into the prompt argument and its ledger record:
// the checked text with the report instruction under a fresh marker. An empty
// flag is no brief. The @path form is read here only; an in-process caller
// passes text to composeLaunchBrief.
func launchBrief(arg string, now func() time.Time) (string, *worker.Brief, error) {
	if arg == "" {
		return "", nil, nil
	}
	text, err := readBriefArg(arg)
	if err != nil {
		return "", nil, err
	}
	return composeLaunchBrief(text, now)
}

// composeLaunchBrief checks brief text and composes it with the report
// instruction under a fresh marker. Empty text is refused by the check.
func composeLaunchBrief(text string, now func() time.Time) (string, *worker.Brief, error) {
	if err := worker.CheckBrief(text, worker.ViaLaunch); err != nil {
		return "", nil, err
	}
	marker, err := worker.NewMarker()
	if err != nil {
		return "", nil, err
	}
	b := &worker.Brief{Marker: marker, Count: 1, SentAt: now().UTC(), Via: worker.ViaLaunch}
	return worker.Compose(text, marker, worker.ViaLaunch), b, nil
}

// workerPlanInputs are the pieces runWorkerLaunch has already built when it
// reaches a --dry-run.
type workerPlanInputs struct {
	spec     workerSpec
	injected map[string]string
	unset    []string
	prompt   string
	hasBrief bool
	// self is this forgectl executable, which the harness binary must not be.
	self string
}

// planWorkerLaunch is runWorkerLaunch's --dry-run: the checks the launch runs
// before its first write, then a preview. It does not open the ledger for
// writing, create the worktree root, run git worktree add, or start a surface.
// A name already in the ledger, a branch or path the launch would refuse, and
// a harness profile that does not build as a worker all fail here as they
// fail there.
func planWorkerLaunch(cmd *cobra.Command, deps module.Deps, opts surfaceLaunchOptions, top string, led *worker.Ledger, in workerPlanInputs) error {
	taken, err := led.NameTaken(opts.DisplayName)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	if taken {
		// The launch refuses it in Begin, with this error and no exit code.
		return fmt.Errorf("%w (--name %q); close it first or pick another name", worker.ErrNameTaken, opts.DisplayName)
	}
	plan, err := worker.PlanWorktree(cmd.Context(), deps.Runner, top, opts.DisplayName, opts.Worktree)
	if err != nil {
		return err
	}
	built, err := buildWorkerInvocation(in.spec.request(deps.Cfg.Launch, plan.Path, in.injected, in.unset), in.prompt, worker.NewSessionID, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if !built.Worker {
		return errors.New("forgectl: the worker invocation was not built as a worker launch")
	}
	// The service refuses an unaccepted binary when it launches; ask the same
	// policy here so the preview refuses too.
	if err := (surface.Policy{AllowPATHBinary: opts.AllowPATH}).AcceptBinary(built.Invocation.Binary, in.self); err != nil {
		return err
	}
	posture, order := workerPosturePlan(built.WorkerPosture)
	return renderLaunchPlan(cmd.OutOrStdout(), launchPlan{
		DryRun:  true,
		Surface: opts.Backend,
		Name:    opts.DisplayName,
		Target:  top,
		Harness: built.Invocation.Harness,
		Worker: &workerLaunchPlan{
			Repo:         top,
			Worktree:     plan.Path,
			Branch:       plan.Branch,
			BranchFrom:   plan.BranchFrom,
			LedgerRow:    ledgerRowWouldCreate,
			Brief:        in.hasBrief,
			Posture:      posture,
			postureOrder: order,
		},
	}, opts.JSON)
}
