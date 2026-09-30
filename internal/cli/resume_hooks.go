package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/doctor"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Seams for `resume hooks`, so no test touches the real LaunchAgents
// directory, launchd, forgectl's state directory, or the test binary's own
// go-build path.
var (
	hooksDir        = config.ResumeHooksDir
	hooksAgentsDir  = defaultAgentsDir
	hooksExecutable = os.Executable
	hooksLookPath   = osexec.LookPath
	hooksLookupEnv  = os.LookupEnv
	hooksUID        = os.Getuid
	hooksGOOS       = runtime.GOOS
	hooksStat       = os.Stat
	// hooksSettle overrides the settle delay; zero means the default.
	hooksSettle time.Duration
	// hooksHandleSignals makes `run` own SIGINT, SIGTERM, and SIGHUP; tests
	// that do not send a signal turn it off so no process-wide handler is
	// installed under `go test`.
	hooksHandleSignals = true
)

// hooksHarnesses are the harnesses `hooks run` watches. Only claude has
// installed-version detection today; config validation refuses the others
// by name. A new harness needs a version reader, a restart action that
// targets its sessions, and watch paths in hooksAgentSpec.
var hooksHarnesses = []string{"claude"}

// Environment the watcher is given.
const (
	// envClaudeBin pins the claude the watcher reads to the one it watches.
	envClaudeBin = "FORGECTL_CLAUDE_BIN"
	// envHerdrBin is the absolute herdr the restart action runs.
	envHerdrBin = "FORGECTL_HERDR_BIN"
	// envHerdrSocket keeps the restart on the herdr server install ran in.
	envHerdrSocket = "HERDR_SOCKET_PATH"
	// envXPCService is set by launchd to the job's label.
	envXPCService = "XPC_SERVICE_NAME"
)

// hookInterval is the watcher's StartInterval, in seconds: a run every 30
// minutes retries an incomplete restart and catches a missed watch event.
// Each run with nothing to do is a symlink read.
const hookInterval = 30 * 60

// hookLogMaxBytes is where `hooks run` rotates the watcher log.
const hookLogMaxBytes = 1 << 20

// hookStatusRuns is how many audit records `hooks status` shows.
const hookStatusRuns = 5

// hookStuckMargin is added to the longest hook timeout before the doctor
// calls a still-running watcher run stuck.
const hookStuckMargin = 10 * time.Minute

func defaultAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func newResumeHooksCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Run [[resume.on_update]] hooks when a harness's installed version changes",
		Long: `hooks runs the [[resume.on_update]] entries in config.toml when the installed
claude changes version, so an update can restart outdated sessions (or run
any command) with nothing typed.

  forgectl resume hooks run --dry-run   show the recorded and installed versions and what would fire
  forgectl resume hooks run             detect a change and fire the hooks (what the watcher runs)
  forgectl resume hooks install         install the launchd watcher (macOS)
  forgectl resume hooks status          installed? loaded? the last runs
  forgectl resume hooks uninstall       remove the watcher

Configure hooks in config.toml (a top-level [[on_update]] is refused and
pointed here). Each names a harness and exactly one of the built-in restart
action or a command (an argv array, run with no shell):

  [[resume.on_update]]
  harness = "claude"
  action  = "restart"          # = forgectl resume restart --outdated

  [[resume.on_update]]
  harness = "claude"
  command = ["/usr/bin/say", "claude updated"]
  timeout_seconds = 60         # optional; default 300 (restart: 1800)

For a command, timeout_seconds bounds the whole run: at the deadline its
process group is killed. For the restart action it bounds only the waiting
for sessions to go idle; a session already stopped is still relaunched.

A command hook gets FORGECTL_HARNESS, FORGECTL_OLD_VERSION, and
FORGECTL_NEW_VERSION in its environment; they are never put in its argv.
Only harness = "claude" is supported; codex and pi are refused by name.

` + "`run`" + ` reads the installed version the way ` + "`resume outdated`" + ` does and compares
it with the last version it recorded (resume-hooks/state.json in forgectl's
config directory). The first run records a baseline and fires nothing. A
change fires only once the version has settled: two reads a few seconds
apart must agree, so a symlink that moves twice, or reverts, during one
update fires nothing extra. Hooks run in config order; one failing does not
stop the next. The new version is recorded only after every hook ran. A run
stopped by SIGTERM, SIGINT, or SIGHUP (launchd sends SIGTERM on unload)
stops the restart's waiting, starts no further hook, and records nothing, so
the next run fires the whole set again — write hooks that are safe to
repeat. After acting, a run re-reads the version and handles an update that
landed meanwhile. Runs are serialized by a lock (a manual run behind the
watcher's prints that it is waiting), and a restart still takes restart's
own lock.

A restart that ends incomplete (a session still busy at its timeout, a
failed relaunch, another restart running) is retried on later runs while the
version is unchanged, up to 3 attempts in all; command hooks fire once per
version and are never retried.

Each hook run appends one line to resume-hooks/runs.jsonl: the time (UTC),
harness, both versions, which hook (its position, and for a command only
the program's base name — never its arguments), the outcome, exit status,
duration, and trigger (launchd or manual). install and uninstall append a
line too. Of a failed command's output only a short tail of stderr is kept,
with argument values scrubbed and URL-credential lines withheld; a bare
token the program prints can still survive in it (the file is 0600). A hook
that succeeds keeps none.

` + "`install`" + ` writes ~/Library/LaunchAgents/` + resume.HooksAgentLabel + `.plist and loads it.
launchd runs ` + "`forgectl resume hooks run`" + ` when the claude symlink or its
directory changes, once at load, and every 30 minutes. launchd gives the job
no shell environment, so install bakes in absolute paths: this forgectl
(refusing a ` + "`go run`" + ` build), the claude it watches (FORGECTL_CLAUDE_BIN, so the
watcher reads the same claude), herdr (FORGECTL_HERDR_BIN), a PATH of their
directories ahead of the system ones, and HERDR_SOCKET_PATH when set. It
warns when the claude it watches is not a versions symlink, and when a
baked directory or binary is writable by another user. Re-run install after
moving any of them. Output goes to resume-hooks/watcher.log. install and
uninstall are safe to repeat.

A restart run by the watcher has no terminal: its progress lines go to the
log, and its outcome to the audit trail. It still never stops a session it
is running inside, never stops a busy one, and waits for idle up to the
hook's timeout.

Exit 0 when nothing fired or every hook ended ok; 1 when any hook failed,
the run was interrupted, or the version could not be read.`,
	}
	cmd.AddCommand(newResumeHooksRunCmd(deps), newResumeHooksInstallCmd(deps), newResumeHooksUninstallCmd(deps), newResumeHooksStatusCmd(deps))
	return cmd
}

func newResumeHooksRunCmd(deps module.Deps) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:           "run",
		Short:         "Fire the configured hooks if a harness's installed version changed",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runResumeHooks(ctx, cmd.OutOrStdout(), deps, dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the recorded and installed versions and the hooks that would fire; run and record nothing")
	return cmd
}

// hooksTrigger names what started this run: launchd sets XPC_SERVICE_NAME
// to the job's label.
func hooksTrigger() string {
	if v, ok := hooksLookupEnv(envXPCService); ok && v == resume.HooksAgentLabel {
		return "launchd"
	}
	// A Claude Code session exports these into every command it runs.
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID"} {
		if v, ok := hooksLookupEnv(k); ok && v != "" {
			return "agent"
		}
	}
	return "manual"
}

func runResumeHooks(ctx context.Context, out io.Writer, deps module.Deps, dryRun bool) error {
	cfgHooks, err := deps.Cfg.ResumeHooks()
	if err != nil {
		return err
	}
	dir, err := hooksDir()
	if err != nil {
		return fmt.Errorf("locate forgectl's state directory: %w", termsafe.Error(err))
	}
	if !dryRun {
		rotateHookLog(filepath.Join(dir, resume.HookLogName))
		if hooksHandleSignals {
			// One signal context for the whole run, shared by the restart
			// action (HandleSignals off there), so a SIGTERM both stops the
			// restart's waiting and tells RunHooks to record nothing.
			var stop context.CancelFunc
			ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			// A closed log pipe must not kill a run between a stop and its
			// relaunch; writes then fail with EPIPE and are dropped.
			signal.Ignore(syscall.SIGPIPE)
			defer signal.Reset(syscall.SIGPIPE)
		}
	}
	specs := resume.HookSpecs(cfgHooks)
	failed := false
	var errs []error
	for _, harness := range hooksHarnesses {
		req := resume.HooksRequest{
			Harness: harness, Hooks: specs, Dir: dir, DryRun: dryRun,
			Trigger:     hooksTrigger(),
			ReadVersion: func(ctx context.Context) (string, error) { return installedVersionFn(ctx, deps) },
			Runner:      deps.Runner,
			Restart:     hookRestart(deps, out, harness),
			Log:         out,
			SettleDelay: hooksSettle,
		}
		res, err := resume.RunHooks(ctx, req)
		if err != nil {
			// One harness's failure does not skip the others.
			errs = append(errs, err)
			continue
		}
		if dryRun {
			if err := printHooksPreview(ctx, out, deps, harness, specs, res); err != nil {
				return err
			}
		}
		failed = failed || res.Failed()
	}
	if len(errs) > 0 {
		return WithExitCode(errors.Join(errs...), 1)
	}
	if failed {
		return WithExitCode(errors.New("a hook did not end ok — see the lines above, or `forgectl resume hooks status`"), 1)
	}
	return nil
}

// hookHerdrBin is the absolute herdr the watcher baked in, or "" to run
// `herdr` from PATH.
func hookHerdrBin() (string, error) {
	v, ok := hooksLookupEnv(envHerdrBin)
	if !ok || v == "" {
		return "", nil
	}
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("%s=%s is not an absolute path", envHerdrBin, termsafe.QuotePath(v))
	}
	return v, nil
}

// hookRestart is the built-in restart action for harness: the same run
// `resume restart --outdated` makes, with its progress on out (the watcher's
// log). It shares the run's context — HandleSignals is off, because the run
// owns the signals — so a SIGTERM ends its waiting and the run records
// nothing.
func hookRestart(deps module.Deps, out io.Writer, harness string) resume.RestartFunc {
	return func(ctx context.Context, timeout time.Duration) (resume.RestartResult, error) {
		if harness != "claude" {
			return resume.RestartResult{}, fmt.Errorf("the restart action supports claude only, not %s", termsafe.SafeLine(harness))
		}
		herdr, err := hookHerdrBin()
		if err != nil {
			return resume.RestartResult{}, err
		}
		paths, err := resumePaths()
		if err != nil {
			return resume.RestartResult{}, fmt.Errorf("locate session records: %w", err)
		}
		installed, err := installedVersionFn(ctx, deps)
		if err != nil {
			return resume.RestartResult{}, err
		}
		req := resume.RestartRequest{
			Paths: paths, Installed: installed,
			Runner:   deps.Runner,
			HerdrBin: herdr,
			Options:  resume.RestartOptions{Timeout: timeout},
			Progress: func(ev resume.RestartEvent) { printRestartEvent(out, ev) },
		}
		if restartOverride != nil {
			restartOverride(&req)
		}
		return resume.RestartOutdated(ctx, req)
	}
}

// printHooksPreview renders --dry-run's answer for one harness.
func printHooksPreview(ctx context.Context, out io.Writer, deps module.Deps, harness string, specs []resume.HookSpec, res resume.HooksResult) error {
	var configured []resume.HookSpec
	for _, h := range specs {
		if h.Harness == harness {
			configured = append(configured, h)
		}
	}
	if len(configured) == 0 {
		_, err := fmt.Fprintf(out, "%s: no [[resume.on_update]] hooks configured\n", harness)
		return err
	}
	firing := map[string]bool{}
	for _, h := range res.Planned {
		firing[h.Identity()] = true
	}
	for _, h := range configured {
		verb := "would not fire"
		if firing[h.Identity()] {
			verb = "would fire"
		}
		if _, err := fmt.Fprintln(out, safeTerm(fmt.Sprintf("%s: %s hook %s (timeout %s)", harness, verb, h.Identity(), h.Timeout))); err != nil {
			return err
		}
	}
	for _, h := range res.Planned {
		if h.Action != config.OnUpdateActionRestart {
			continue
		}
		preview, err := restartPreview(ctx, deps)
		if err != nil {
			_, werr := fmt.Fprintln(out, safeTerm("restart preview failed: "+err.Error()))
			return werr
		}
		if err := printRestartPreview(out, preview); err != nil {
			return err
		}
		break
	}
	return nil
}

// restartPreview is `resume restart --outdated --dry-run`: reads only.
func restartPreview(ctx context.Context, deps module.Deps) ([]resume.PreviewLine, error) {
	herdr, err := hookHerdrBin()
	if err != nil {
		return nil, err
	}
	paths, err := resumePaths()
	if err != nil {
		return nil, fmt.Errorf("locate session records: %w", err)
	}
	installed, err := installedVersionFn(ctx, deps)
	if err != nil {
		return nil, err
	}
	req := resume.RestartRequest{Paths: paths, Installed: installed, DryRun: true, Runner: deps.Runner, HerdrBin: herdr}
	if restartOverride != nil {
		restartOverride(&req)
	}
	res, err := resume.RestartOutdated(ctx, req)
	return res.Preview, err
}

// rotateHookLog moves the watcher log aside once it passes hookLogMaxBytes.
// A rename rather than a truncate: this run's own stdout (opened by launchd)
// keeps writing to the moved file, and launchd opens a fresh watcher.log at
// the next spawn, so nothing depends on how launchd opened it. Failure only
// means a larger log, so it is ignored.
func rotateHookLog(path string) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() > hookLogMaxBytes {
		_ = os.Rename(path, path+".1") // best effort; see above
	}
}

func newResumeHooksInstallCmd(deps module.Deps) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:           "install",
		Short:         "Install the launchd agent that runs `resume hooks run` when claude updates (macOS)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return runResumeHooksInstall(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), deps, dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plist that would be installed; write and load nothing")
	return cmd
}

// writableByOthers reports a file or directory another user could replace:
// group- or other-writable (group members can write it even when this user
// owns it, as with Homebrew's admin-group bin), owned by a user other than
// this one or root, or with an owner that could not be read. It is the check
// the baked paths get, since the watcher runs them unattended.
func writableByOthers(mode fs.FileMode, owner, uid int, ownerKnown bool) bool {
	if mode.Perm()&0o022 != 0 || !ownerKnown {
		return true
	}
	return owner != uid && owner != 0
}

// hooksAgentSpec resolves everything the plist needs, now, from this
// process's environment — the one the job will not have.
func hooksAgentSpec(deps module.Deps, warn io.Writer) (resume.AgentSpec, error) {
	exe, err := hooksExecutable()
	if err != nil {
		return resume.AgentSpec{}, fmt.Errorf("locate this forgectl binary: %w", err)
	}
	if err := resume.CheckAgentBinary(exe); err != nil {
		return resume.AgentSpec{}, err
	}
	lc, lcSource := resolveLaunchConfig(deps.LegacyBoundary, deps.Cfg, "")
	claude, err := launch.ResolveBinary("claude", lc.Defaults)
	if err != nil {
		return resume.AgentSpec{}, fmt.Errorf("locate the claude binary to watch: %w", err)
	}
	claudePath := claude.Path
	_, _ = fmt.Fprintln(warn, safeTerm(fmt.Sprintf("forgectl: watching claude at %s (chosen by %s; launch config from %s)", termsafe.QuotePath(claudePath), claude.Source, lcSource)))
	if err := resume.CheckVersionsLink(claudePath); err != nil {
		_, _ = fmt.Fprintln(warn, safeTerm("forgectl: WARNING: "+claudePath+" is not a symlink into a versions directory ("+err.Error()+"); an update will not touch it, so the watcher will notice updates only at load and every 30 minutes"))
	}
	dir, err := hooksDir()
	if err != nil {
		return resume.AgentSpec{}, fmt.Errorf("locate forgectl's state directory: %w", termsafe.Error(err))
	}
	env := map[string]string{envClaudeBin: claudePath}
	pathDirs := []string{filepath.Dir(claudePath)}
	baked := []string{exe, claudePath}
	if herdr, err := hooksLookPath("herdr"); err == nil {
		if abs, err := filepath.Abs(herdr); err == nil {
			env[envHerdrBin] = abs
			pathDirs = append(pathDirs, filepath.Dir(abs))
			baked = append(baked, abs)
		}
	} else {
		_, _ = fmt.Fprintln(warn, "forgectl: herdr is not on PATH; the restart action will fail under the watcher until it is and install is re-run")
	}
	pathDirs = append(pathDirs, filepath.Dir(exe))
	env["PATH"] = resume.AgentPATH(pathDirs...)
	if v, ok := hooksLookupEnv(envHerdrSocket); ok && v != "" {
		env[envHerdrSocket] = v
	}
	warnWritable(warn, append(baked, pathDirs...))
	return resume.AgentSpec{
		Label:   resume.HooksAgentLabel,
		Program: exe,
		Args:    []string{"resume", "hooks", "run"},
		// The directory as well as the link: launchd watches a path through
		// the vnode it opens, and opening a symlink opens its target, so an
		// update that replaces the link (a rename in its directory) may
		// never touch the vnode watched through it. The directory's entry
		// change is certain; a change there that is not an update costs one
		// quick run that finds the version unchanged.
		WatchPaths: []string{claudePath, filepath.Dir(claudePath)},
		Env:        env,
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, resume.HookLogName),
		Interval:   hookInterval,
	}, nil
}

// warnWritable warns once per path another user could replace.
func warnWritable(warn io.Writer, paths []string) {
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		fi, err := hooksStat(p)
		if err != nil {
			continue
		}
		owner, known := fileOwner(fi)
		if writableByOthers(fi.Mode(), owner, hooksUID(), known) {
			_, _ = fmt.Fprintln(warn, safeTerm("forgectl: WARNING: "+p+" can be changed by another user (group- or other-writable, or owned by someone else); the watcher runs what is there unattended"))
		}
	}
}

func runResumeHooksInstall(ctx context.Context, out, warn io.Writer, deps module.Deps, dryRun bool) error {
	if hooksGOOS != "darwin" {
		return errors.New("the update-hooks watcher is a launchd agent and installs only on macOS; run `forgectl resume hooks run` from your own scheduler instead")
	}
	cfgHooks, err := deps.Cfg.ResumeHooks()
	if err != nil {
		return err
	}
	spec, err := hooksAgentSpec(deps, warn)
	if err != nil {
		return err
	}
	if dryRun {
		data, err := resume.RenderAgentPlist(spec)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if _, err := fmt.Fprintln(out, safeTerm(line)); err != nil {
				return err
			}
		}
		return nil
	}
	agents, err := hooksAgentsDir()
	if err != nil {
		return fmt.Errorf("locate the LaunchAgents directory: %w", termsafe.Error(err))
	}
	plist := resume.AgentPlistPath(agents, spec.Label)
	outcome, err := resume.InstallAgent(ctx, agents, spec, resume.Launchctl{Runner: deps.Runner, UID: hooksUID()})
	auditAgentEvent("watcher install", plist, string(outcome), err)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s: %s (watching %s)\n", outcome, termsafe.QuotePath(plist), termsafe.QuotePath(spec.WatchPaths[0])); err != nil {
		return err
	}
	if len(cfgHooks) == 0 {
		_, err = fmt.Fprintln(out, "no [[resume.on_update]] hooks are configured yet: the watcher will record versions and fire nothing")
	}
	return err
}

// auditAgentEvent appends an install or uninstall to the audit trail, so a
// persistence change leaves a record beside the runs it enables. Best
// effort: the change itself already happened (or failed) and is reported.
func auditAgentEvent(event, plist, outcome string, err error) {
	dir, derr := hooksDir()
	if derr != nil {
		return
	}
	rec := resume.HookRun{Time: time.Now().UTC(), Hook: event, Outcome: resume.OutcomeOK, Exit: 0, Trigger: hooksTrigger(), Detail: termsafe.SafeLine(outcome + " " + plist)}
	if err != nil {
		rec.Outcome, rec.Exit, rec.Detail = resume.OutcomeFailed, 1, termsafe.SafeLineMax(err.Error(), 240)
	}
	_ = resume.FileHookStore{Dir: dir}.Append(rec) // best effort; see above
}

func newResumeHooksUninstallCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:           "uninstall",
		Short:         "Unload and remove the update-hooks launchd agent",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if hooksGOOS != "darwin" {
				return errors.New("the update-hooks watcher is a launchd agent and exists only on macOS")
			}
			agents, err := hooksAgentsDir()
			if err != nil {
				return fmt.Errorf("locate the LaunchAgents directory: %w", termsafe.Error(err))
			}
			plist := resume.AgentPlistPath(agents, resume.HooksAgentLabel)
			did, err := resume.UninstallAgent(ctx, agents, resume.HooksAgentLabel, resume.Launchctl{Runner: deps.Runner, UID: hooksUID()})
			if did || err != nil {
				auditAgentEvent("watcher uninstall", plist, "removed", err)
			}
			if err != nil {
				return err
			}
			msg := "removed " + termsafe.QuotePath(plist)
			if !did {
				msg = "not installed; nothing to remove"
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), msg)
			return err
		},
	}
}

func newResumeHooksStatusCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the update-hooks watcher is installed and loaded, and its last runs",
		Long: `status reports the [[resume.on_update]] hook count, whether the launchd watcher
is installed and loaded (and launchd's last exit code for it), the state
recorded per harness (version, and any restart pending a retry), the log
path, and the last few audit records. It reads only: a plist stat,
` + "`launchctl print`" + `, and forgectl's own state files.

--json emits one object: hooks, config_error, watcher (installed, loaded,
state, runs, last_exit, plist; null off macOS), recorded (harness to
{version, pending, attempts}), log, and runs (the audit records, oldest
first).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			st, err := readHooksStatus(ctx, deps)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), st)
			}
			return printHooksStatus(cmd.OutOrStdout(), st)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit one JSON object instead of text")
	return cmd
}

// hooksWatcherDTO is the watcher half of `resume hooks status --json`.
type hooksWatcherDTO struct {
	Installed bool   `json:"installed"`
	Loaded    bool   `json:"loaded"`
	State     string `json:"state"`
	Runs      int    `json:"runs"`
	LastExit  string `json:"last_exit"`
	Plist     string `json:"plist"`
	summary   string
}

// hooksStatusDTO is `resume hooks status --json`.
type hooksStatusDTO struct {
	Hooks       int                            `json:"hooks"`
	ConfigError string                         `json:"config_error,omitempty"`
	Watcher     *hooksWatcherDTO               `json:"watcher"`
	Recorded    map[string]resume.HarnessState `json:"recorded"`
	RecordedErr string                         `json:"recorded_error,omitempty"`
	Log         string                         `json:"log"`
	Runs        []resume.HookRun               `json:"runs"`
	RunsErr     string                         `json:"runs_error,omitempty"`
}

func readHooksStatus(ctx context.Context, deps module.Deps) (hooksStatusDTO, error) {
	var st hooksStatusDTO
	cfgHooks, cfgErr := deps.Cfg.ResumeHooks()
	st.Hooks = len(cfgHooks)
	if cfgErr != nil {
		st.ConfigError = cfgErr.Error()
	}
	if hooksGOOS == "darwin" {
		agents, err := hooksAgentsDir()
		if err != nil {
			return st, fmt.Errorf("locate the LaunchAgents directory: %w", termsafe.Error(err))
		}
		a := resume.ReadAgentStatus(ctx, agents, resume.HooksAgentLabel, resume.Launchctl{Runner: deps.Runner, UID: hooksUID()})
		st.Watcher = &hooksWatcherDTO{Installed: a.Installed, Loaded: a.Loaded, State: a.State, Runs: a.Runs, LastExit: a.LastExit, Plist: a.PlistPath, summary: describeAgent(a)}
	}
	dir, err := hooksDir()
	if err != nil {
		return st, fmt.Errorf("locate forgectl's state directory: %w", termsafe.Error(err))
	}
	store := resume.FileHookStore{Dir: dir}
	st.Recorded, err = store.States()
	if err != nil {
		st.RecordedErr = err.Error()
	}
	if st.Recorded == nil {
		st.Recorded = map[string]resume.HarnessState{}
	}
	st.Log = filepath.Join(dir, resume.HookLogName)
	st.Runs, err = store.RecentRuns(hookStatusRuns)
	if err != nil {
		st.RunsErr = err.Error()
	}
	if st.Runs == nil {
		st.Runs = []resume.HookRun{}
	}
	return st, nil
}

// printHooksStatus renders the text form. Every line goes through safeTerm:
// the recorded state and audit records are read back from disk.
func printHooksStatus(out io.Writer, st hooksStatusDTO) error {
	lines := []string{fmt.Sprintf("config:     %d [[resume.on_update]] hook(s)", st.Hooks)}
	if st.ConfigError != "" {
		lines[0] = "config:     " + st.ConfigError
	}
	if st.Watcher != nil {
		lines = append(lines, "watcher:    "+st.Watcher.summary)
	} else {
		lines = append(lines, "watcher:    none (launchd agents are macOS-only)")
	}
	switch {
	case st.RecordedErr != "":
		lines = append(lines, "recorded:   "+st.RecordedErr)
	case len(st.Recorded) == 0:
		lines = append(lines, "recorded:   none yet (the first run records a baseline)")
	default:
		for _, h := range hooksHarnesses {
			if v, ok := st.Recorded[h]; ok {
				line := "recorded:   " + h + " " + v.Version
				if len(v.Pending) > 0 {
					line += fmt.Sprintf("; restart pending (%s), %d of %d attempts made", strings.Join(v.Pending, ", "), v.Attempts, resume.MaxRestartAttempts)
				}
				lines = append(lines, line)
			}
		}
	}
	lines = append(lines, "log:        "+termsafe.QuotePath(st.Log))
	switch {
	case st.RunsErr != "":
		lines = append(lines, "last runs:  "+st.RunsErr)
	case len(st.Runs) == 0:
		lines = append(lines, "last runs:  none")
	default:
		lines = append(lines, "last runs:")
		for _, r := range st.Runs {
			lines = append(lines, fmt.Sprintf("  %s  %s %s -> %s  %s  %s exit %d  %dms  %s%s",
				r.Time.UTC().Format(time.RFC3339), r.Harness, r.Old, r.New, r.Hook, r.Outcome, r.Exit, r.DurationMS, r.Trigger, detailOf(r.Detail)))
		}
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, safeTerm(l)); err != nil {
			return err
		}
	}
	return nil
}

func detailOf(d string) string {
	if d == "" {
		return ""
	}
	return "  " + d
}

// describeAgent is one line for status and the doctor row.
func describeAgent(st resume.AgentStatus) string {
	switch {
	case !st.Installed && !st.Loaded:
		return "not installed (run `forgectl resume hooks install`)"
	case !st.Installed:
		return "loaded, but " + termsafe.QuotePath(st.PlistPath) + " is missing (run `forgectl resume hooks install` again)"
	case !st.Loaded:
		return "installed at " + termsafe.QuotePath(st.PlistPath) + " but not loaded (run `forgectl resume hooks install` again)"
	}
	s := "installed and loaded"
	if st.State != "" {
		s += ", " + st.State
	}
	if st.Runs >= 0 {
		s += fmt.Sprintf(", %d run(s)", st.Runs)
	}
	if st.LastExit != "" {
		s += ", last exit " + st.LastExit
	}
	return s
}

// hooksDoctorFacts is what the doctor row decides on.
type hooksDoctorFacts struct {
	Agent resume.AgentStatus
	// State is the recorded claude state; HaveState is false before any run.
	State     resume.HarnessState
	HaveState bool
	// LastRun is the newest audit record for the recorded version.
	LastRun     resume.HookRun
	HaveLastRun bool
	// InFlightSince is when the run holding the hooks lock started.
	InFlightSince time.Time
	InFlight      bool
	Now           time.Time
}

// hooksDoctorProbe is the read-only probe `launch doctor` uses; a seam so
// the doctor's tests never depend on this machine's launchd or state.
var hooksDoctorProbe = func(ctx context.Context) (hooksDoctorFacts, error) {
	agents, err := hooksAgentsDir()
	if err != nil {
		return hooksDoctorFacts{}, err
	}
	f := hooksDoctorFacts{Now: time.Now()}
	f.Agent = resume.ReadAgentStatus(ctx, agents, resume.HooksAgentLabel, resume.Launchctl{Runner: exec.OSRunner{}, UID: hooksUID()})
	dir, err := hooksDir()
	if err != nil {
		return f, nil
	}
	store := resume.FileHookStore{Dir: dir}
	if st, ok, err := store.Load("claude"); err == nil && ok {
		f.State, f.HaveState = st, true
		f.LastRun, f.HaveLastRun, _ = store.LastRunFor("claude", st.Version)
	}
	_, f.InFlightSince, f.InFlight = store.InFlight()
	return f, nil
}

// hooksLongestTimeout is the longest configured hook timeout.
func hooksLongestTimeout(hooks []config.OnUpdateHook) time.Duration {
	var longest time.Duration
	for _, h := range resume.HookSpecs(hooks) {
		longest = max(longest, h.Timeout)
	}
	return longest
}

// hooksDoctorRow is the `launch doctor` verdict on the watcher. It warns,
// never fails: the watcher is optional. launchd's own last exit code is not
// used as the failure signal, because the directory watch starts many runs
// with nothing to do, and each one's exit 0 overwrites a failure. The audit
// trail is the signal instead, plus a run still holding the hooks lock past
// the longest hook timeout (a job stuck on a permission prompt or a hung
// call still reads as loaded).
func hooksDoctorRow(configured int, longest time.Duration, cfgErr error, f hooksDoctorFacts, probeErr error) (doctor.State, string) {
	const prefix = "update-hooks watcher: "
	st := f.Agent
	switch {
	case cfgErr != nil:
		return doctor.StateWarn, prefix + "[[resume.on_update]] is invalid: " + termsafe.SafeLine(cfgErr.Error())
	case probeErr != nil:
		return doctor.StateWarn, prefix + "could not check: " + termsafe.SafeLine(probeErr.Error())
	case !st.Installed && !st.Loaded && configured == 0:
		return doctor.StateOK, prefix + "not installed (no [[resume.on_update]] hooks configured)"
	case !st.Installed || !st.Loaded:
		return doctor.StateWarn, prefix + fmt.Sprintf("%d hook(s) configured; ", configured) + describeAgent(st)
	case f.InFlight && st.State == "running" && f.Now.Sub(f.InFlightSince) > longest+hookStuckMargin:
		return doctor.StateWarn, prefix + fmt.Sprintf("a run has been going since %s, past the longest hook timeout; it may be stuck (a permission prompt, a hung call) — see the watcher log", f.InFlightSince.UTC().Format(time.RFC3339))
	case f.HaveState && len(f.State.Pending) > 0:
		return doctor.StateWarn, prefix + fmt.Sprintf("restart for %s incomplete (%d of %d attempts) — see `forgectl resume hooks status`", termsafe.SafeLine(f.State.Version), f.State.Attempts, resume.MaxRestartAttempts)
	case f.HaveLastRun && f.LastRun.Outcome != resume.OutcomeOK:
		return doctor.StateWarn, prefix + fmt.Sprintf("hook %s for %s ended %s — see `forgectl resume hooks status`", termsafe.SafeLine(f.LastRun.Hook), termsafe.SafeLine(f.LastRun.New), termsafe.SafeLine(f.LastRun.Outcome))
	}
	return doctor.StateOK, prefix + describeAgent(st)
}
