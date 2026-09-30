package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	// hooksSettle overrides the settle delay; zero means the default.
	hooksSettle time.Duration
)

// hooksHarnesses are the harnesses `hooks run` watches. Only claude has
// installed-version detection today; config validation refuses the others
// by name, so adding one is this list plus its version reader.
var hooksHarnesses = []string{"claude"}

// hooksPassEnv are the variables install copies from its own environment
// into the agent's when they are set: launchd starts the job with none of
// the operator's shell. FORGECTL_CLAUDE_BIN keeps the watcher reading the
// same claude as `forgectl launch`; HERDR_SOCKET_PATH keeps the restart
// talking to the same herdr server as the terminal install ran in.
var hooksPassEnv = []string{"FORGECTL_CLAUDE_BIN", "HERDR_SOCKET_PATH"}

// hookLogMaxBytes is where `hooks run` empties the watcher log before it
// writes: launchd appends to it forever otherwise.
const hookLogMaxBytes = 1 << 20

// hookStatusRuns is how many audit records `hooks status` shows.
const hookStatusRuns = 5

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

Configure hooks in config.toml. Each names a harness and exactly one of the
built-in restart action or a command (an argv array, run with no shell):

  [[resume.on_update]]
  harness = "claude"
  action  = "restart"          # = forgectl resume restart --outdated

  [[resume.on_update]]
  harness = "claude"
  command = ["/usr/bin/say", "claude updated"]
  timeout_seconds = 60         # optional; default 300 (restart: 1800)

A command hook gets FORGECTL_HARNESS, FORGECTL_OLD_VERSION, and
FORGECTL_NEW_VERSION in its environment; they are never put in its argv.
Only harness = "claude" is supported; codex and pi are refused by name.

` + "`run`" + ` reads the installed version the way ` + "`resume outdated`" + ` does and compares
it with the last version it recorded (in forgectl's state directory,
resume-hooks/state.json). The first run records a baseline and fires
nothing. A change fires only once the version has settled: two reads a few
seconds apart must agree, so a symlink that moves twice, or reverts, during
one update fires nothing extra. Hooks run in config order; one failing does
not stop the next. The new version is recorded only after every hook ran,
so a run killed partway fires again on the next trigger rather than
skipping the update — write hooks that are safe to repeat. Runs are
serialized by a lock, and a restart still takes restart's own lock.

Each hook run appends one line to resume-hooks/runs.jsonl: the time (UTC),
harness, both versions, which hook (its position, and for a command only
the program's base name — never its arguments), the outcome, exit status,
and duration. Of a failed command's output only a short, redacted tail of
stderr is kept; a hook that succeeds keeps none.

` + "`install`" + ` writes ~/Library/LaunchAgents/` + resume.HooksAgentLabel + `.plist and loads it.
launchd runs ` + "`forgectl resume hooks run`" + ` when the claude symlink (or its directory)
changes, and once at load. launchd gives the job no shell environment, so
install resolves what it needs now: this forgectl by absolute path (refusing
a ` + "`go run`" + ` build, which is deleted when it exits), a PATH holding the directories
of herdr, claude, and forgectl, and FORGECTL_CLAUDE_BIN and HERDR_SOCKET_PATH
when set. Re-run install after moving any of them. Its output goes to
resume-hooks/watcher.log. install and uninstall are safe to repeat.

A restart run by the watcher has no terminal: its progress lines go to the
log, and its outcome to the audit trail. It still never stops a session it
is running inside, never stops a busy one, and waits for idle up to the
hook's timeout.

Exit 0 when nothing fired or every hook ended ok; 1 when any hook failed or
the version could not be read.`,
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
		trimHookLog(filepath.Join(dir, resume.HookLogName))
	}
	specs := resume.HookSpecs(cfgHooks)
	failed := false
	for _, harness := range hooksHarnesses {
		req := resume.HooksRequest{
			Harness: harness, Hooks: specs, Dir: dir, DryRun: dryRun,
			ReadVersion: func(ctx context.Context) (string, error) { return installedVersionFn(ctx, deps) },
			Runner:      deps.Runner,
			Restart:     hookRestart(deps, out),
			Log:         out,
			SettleDelay: hooksSettle,
		}
		res, err := resume.RunHooks(ctx, req)
		if err != nil {
			return err
		}
		if dryRun {
			if err := printHooksPreview(ctx, out, deps, harness, specs, res); err != nil {
				return err
			}
		}
		failed = failed || res.Failed()
	}
	if failed {
		return WithExitCode(errors.New("a hook did not end ok — see the lines above, or `forgectl resume hooks status`"), 1)
	}
	return nil
}

// hookRestart is the built-in restart action: the same run `resume restart
// --outdated` makes, with its progress on out (the watcher's log).
func hookRestart(deps module.Deps, out io.Writer) resume.RestartFunc {
	return func(ctx context.Context, timeout time.Duration) (resume.RestartResult, error) {
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
			Runner:        deps.Runner,
			Options:       resume.RestartOptions{Timeout: timeout},
			Progress:      func(ev resume.RestartEvent) { printRestartEvent(out, ev) },
			HandleSignals: true,
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
	verb := "would fire"
	if len(res.Planned) == 0 {
		verb = "would not fire"
	}
	for _, h := range configured {
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
	paths, err := resumePaths()
	if err != nil {
		return nil, fmt.Errorf("locate session records: %w", err)
	}
	installed, err := installedVersionFn(ctx, deps)
	if err != nil {
		return nil, err
	}
	req := resume.RestartRequest{Paths: paths, Installed: installed, DryRun: true, Runner: deps.Runner}
	if restartOverride != nil {
		restartOverride(&req)
	}
	res, err := resume.RestartOutdated(ctx, req)
	return res.Preview, err
}

// trimHookLog empties the watcher log once it passes hookLogMaxBytes. launchd
// holds it open for append, so its next write lands at the new end. Failure
// only means a larger log, so it is ignored.
func trimHookLog(path string) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() > hookLogMaxBytes {
		_ = os.Truncate(path, 0) // best effort; see above
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
	lc, _ := resolveLaunchConfig(deps.LegacyBoundary, deps.Cfg, "")
	claudePath, err := launch.ClaudePath(lc.Defaults)
	if err != nil {
		return resume.AgentSpec{}, fmt.Errorf("locate the claude binary to watch: %w", err)
	}
	dir, err := hooksDir()
	if err != nil {
		return resume.AgentSpec{}, fmt.Errorf("locate forgectl's state directory: %w", termsafe.Error(err))
	}
	pathDirs := []string{}
	if herdr, err := hooksLookPath("herdr"); err == nil {
		if abs, err := filepath.Abs(herdr); err == nil {
			pathDirs = append(pathDirs, filepath.Dir(abs))
		}
	} else {
		_, _ = fmt.Fprintln(warn, "forgectl: herdr is not on PATH; the restart action will fail under the watcher until it is and install is re-run")
	}
	pathDirs = append(pathDirs, filepath.Dir(claudePath), filepath.Dir(exe))
	env := map[string]string{"PATH": resume.AgentPATH(pathDirs...)}
	for _, k := range hooksPassEnv {
		if v, ok := hooksLookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
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
	}, nil
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
	outcome, err := resume.InstallAgent(ctx, agents, spec, resume.Launchctl{Runner: deps.Runner, UID: hooksUID()})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s: %s (watching %s)\n", outcome, termsafe.QuotePath(resume.AgentPlistPath(agents, spec.Label)), termsafe.QuotePath(spec.WatchPaths[0])); err != nil {
		return err
	}
	if len(cfgHooks) == 0 {
		_, err = fmt.Fprintln(out, "no [[resume.on_update]] hooks are configured yet: the watcher will record versions and fire nothing")
	}
	return err
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
			did, err := resume.UninstallAgent(ctx, agents, resume.HooksAgentLabel, resume.Launchctl{Runner: deps.Runner, UID: hooksUID()})
			if err != nil {
				return err
			}
			msg := "removed " + termsafe.QuotePath(resume.AgentPlistPath(agents, resume.HooksAgentLabel))
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
is installed and loaded (and launchd's last exit code for it), the version
recorded per harness, the log path, and the last few hook runs. It reads
only: a plist stat, ` + "`launchctl print`" + `, and forgectl's own state files.

--json emits one object: hooks, config_error, watcher (installed, loaded,
state, runs, last_exit, plist; null off macOS), recorded (harness to
version), log, and runs (the audit records, oldest first).`,
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
	Hooks       int               `json:"hooks"`
	ConfigError string            `json:"config_error,omitempty"`
	Watcher     *hooksWatcherDTO  `json:"watcher"`
	Recorded    map[string]string `json:"recorded"`
	RecordedErr string            `json:"recorded_error,omitempty"`
	Log         string            `json:"log"`
	Runs        []resume.HookRun  `json:"runs"`
	RunsErr     string            `json:"runs_error,omitempty"`
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
	st.Recorded, err = store.RecordedVersions()
	if err != nil {
		st.RecordedErr = err.Error()
	}
	if st.Recorded == nil {
		st.Recorded = map[string]string{}
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
// the recorded versions and audit records are read back from disk.
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
				lines = append(lines, "recorded:   "+h+" "+v)
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
			lines = append(lines, fmt.Sprintf("  %s  %s %s -> %s  %s  %s exit %d  %dms%s",
				r.Time.UTC().Format(time.RFC3339), r.Harness, r.Old, r.New, r.Hook, r.Outcome, r.Exit, r.DurationMS, detailOf(r.Detail)))
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

// hooksDoctorProbe is the read-only probe `launch doctor` uses; a seam so
// the doctor's tests never depend on this machine's launchd.
var hooksDoctorProbe = func(ctx context.Context) (resume.AgentStatus, error) {
	agents, err := hooksAgentsDir()
	if err != nil {
		return resume.AgentStatus{}, err
	}
	return resume.ReadAgentStatus(ctx, agents, resume.HooksAgentLabel, resume.Launchctl{Runner: exec.OSRunner{}, UID: hooksUID()}), nil
}

// hooksDoctorRow is the `launch doctor` verdict on the watcher. A config
// with hooks and no loaded watcher warns: the hooks would never fire. So does
// a last run that exited non-zero, since a job stuck or failing still reads
// as loaded.
func hooksDoctorRow(configured int, cfgErr error, st resume.AgentStatus, probeErr error) (doctor.State, string) {
	const prefix = "update-hooks watcher: "
	switch {
	case cfgErr != nil:
		return doctor.StateWarn, prefix + "[[resume.on_update]] is invalid: " + termsafe.SafeLine(cfgErr.Error())
	case probeErr != nil:
		return doctor.StateWarn, prefix + "could not check: " + termsafe.SafeLine(probeErr.Error())
	case !st.Installed && !st.Loaded && configured == 0:
		return doctor.StateOK, prefix + "not installed (no [[resume.on_update]] hooks configured)"
	case !st.Installed || !st.Loaded:
		return doctor.StateWarn, prefix + fmt.Sprintf("%d hook(s) configured; ", configured) + describeAgent(st)
	case st.LastExit != "" && st.LastExit != "0" && !strings.Contains(st.LastExit, "never"):
		return doctor.StateWarn, prefix + describeAgent(st) + " — see `forgectl resume hooks status`"
	}
	return doctor.StateOK, prefix + describeAgent(st)
}
