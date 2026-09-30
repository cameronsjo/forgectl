// Package doctor is forgectl's ecosystem health check: it orchestrates the
// checks each domain package already knows how to make — claude's presence
// (internal/launch), the local bench's reachability (internal/bench), the
// workflow-blessing trust store (internal/bless), and forgectl's own
// currency against its Homebrew tap (internal/selfupdate) — into one
// report. It reimplements none of them; a broken or missing dependency here
// is a bug in the orchestration, not a reason to duplicate a check that
// already lives closer to its own domain.
//
// Every Check is independent: one failing check never stops the others from
// running, and Report carries all of them so `forgectl doctor` always shows
// the whole picture in one pass.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/selfupdate"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// State is a check's resolved health — a small closed vocabulary, mirroring
// internal/bench's own State shape so the two read consistently side by
// side (bench's components appear as Checks in this same Report).
type State string

const (
	// StateOK: the check passed outright.
	StateOK State = "ok"
	// StateWarn: not broken, but worth a look — a missing optional
	// integration, an available (not yet applied) upgrade.
	StateWarn State = "warn"
	// StateFail: broken and actionable — Report.Healthy() is false when any
	// Check is StateFail.
	StateFail State = "fail"
	// StateSkip: nothing to check on this machine (an optional integration
	// that was never configured) — recorded, not silently omitted.
	StateSkip State = "skip"
)

// Check is one health check's outcome.
type Check struct {
	Name   string
	State  State
	Detail string
	// Hint is an actionable remediation, non-empty only when State is Warn
	// or Fail — every failure in the report names what to do about it.
	Hint string
}

// Report is `doctor`'s full outcome: one Check per probe, in a fixed order.
type Report struct {
	Checks []Check
}

// Healthy reports whether every Check passed or was a non-fatal warn/skip —
// false only when at least one Check is StateFail.
func (r Report) Healthy() bool {
	for _, c := range r.Checks {
		if c.State == StateFail {
			return false
		}
	}
	return true
}

// Deps carries the seams Run needs. NewDeps wires the real production
// values; tests inject fakes for LookPath/TrustedStore/Prober so a check can
// be exercised without a real claude/tmux/ghostty/brew on PATH or a real
// trust store on disk.
type Deps struct {
	Cfg          config.Config
	Runner       exec.Runner
	LookPath     func(string) (string, error)
	TrustedStore func() (bless.Store, error)
	// TrustStorePath resolves where the trust store would live. It lets the
	// trust check tell a machine that never set up blessed workflows (no
	// anchor, no store) from one whose store exists but has lost its anchor.
	// Nil means unknown, and an absent anchor then reads as a failure.
	TrustStorePath func() (string, error)
	Prober         bench.Prober
	// ResumePaths resolves the session-record locations `forgectl resume`
	// reads. Seamed so the task-dialect check can run against a fixture
	// tree rather than the machine's real ~/.claude.
	ResumePaths func() (resume.Paths, error)
}

// NewDeps wires Deps with production seams: os/exec.LookPath, the real
// bless.Verifier's trust-store read, and bench's real HTTP prober.
func NewDeps(cfg config.Config, runner exec.Runner) Deps {
	return Deps{
		Cfg:            cfg,
		Runner:         runner,
		LookPath:       osexec.LookPath,
		TrustedStore:   bless.NewVerifier().TrustedStore,
		TrustStorePath: config.TrustStorePath,
		Prober:         bench.NewHTTPProber(),
		ResumePaths:    resume.DefaultPaths,
	}
}

// Run executes every check and returns the aggregate Report. It never
// returns an error itself — an unhealthy ecosystem is a Report full of
// StateFail Checks, not a Go error; the caller (the CLI layer) decides the
// process exit code from Report.Healthy().
func Run(ctx context.Context, d Deps) Report {
	var checks []Check

	checks = append(checks, checkClaude(d))
	checks = append(checks, checkConfig(d))
	checks = append(checks, checkLogPath(d))
	checks = append(checks, checkBinary(d, "tmux", "tmux not found on PATH — install with `brew install tmux`"))
	checks = append(checks, checkBinary(d, "ghostty", "ghostty not found on PATH — install from https://ghostty.org"))
	checks = append(checks, checkBinary(d, "cmux", "cmux not found on PATH — see https://github.com/cameronsjo/cmux"))
	checks = append(checks, checkMdroll(d))
	checks = append(checks, checkSops(ctx, d))
	checks = append(checks, checkGh(ctx, d))
	checks = append(checks, benchChecks(ctx, d)...)
	checks = append(checks, checkTrustStore(d))
	checks = append(checks, checkResumeTasks(d))
	checks = append(checks, checkForgectlVersion(ctx, d))

	return Report{Checks: checks}
}

// checkClaude reuses launch.ClaudePath — the exact resolution `forgectl
// launch doctor` already reports on (env override, configured binary_path,
// PATH) — rather than re-deriving PATH resolution here.
func checkClaude(d Deps) Check {
	p, err := launch.ClaudePath(d.Cfg.Launch.Defaults)
	if err == nil {
		return Check{Name: "claude", State: StateOK, Detail: p}
	}
	// Categorical (#716): err renders a path from the environment or config
	// and a wrapped filesystem error. The report and --json say which way it
	// failed; the log keeps the rest.
	slog.Warn("claude binary could not be resolved.", "error", err)
	return Check{Name: "claude", State: StateFail, Detail: "claude binary not found or not usable", Hint: "install claude, or set [launch.defaults].binary_path / $FORGECTL_CLAUDE_BIN"}
}

// checkConfig reuses config.Validate() — the exact parse `forgectl launch
// doctor` already surfaces — so a malformed config.toml fails the same way
// in both places.
func checkConfig(d Deps) Check {
	path, pathErr := config.ConfigPath()
	if err := config.Validate(); err != nil {
		return Check{Name: "config", State: StateFail, Detail: err.Error(), Hint: "fix config.toml so it can be read and parsed (see the error above)"}
	}
	if pathErr != nil {
		return Check{Name: "config", State: StateWarn, Detail: pathErr.Error(), Hint: "config directory could not be resolved"}
	}
	return Check{Name: "config", State: StateOK, Detail: path}
}

// checkLogPath resolves where forgectl would write its log (config.
// ResolvedLogPath, the same resolution SetupLogger's openLogWriter applies)
// and reports whether its parent directory already exists — a read-only
// Stat, never a mkdir: a health check must have no side effect of its own,
// and config.OpenAppendFile already creates the directory on demand at the
// first real log write, so doctor has nothing useful to create here. A
// missing directory is StateWarn, not StateFail — it will be created
// automatically the next time forgectl actually logs something.
func checkLogPath(d Deps) Check {
	path := config.ResolvedLogPath(d.Cfg.LogFile)
	switch path {
	case "stderr":
		return Check{Name: "log path", State: StateOK, Detail: "stderr (log_file = \"-\")"}
	case "(unavailable)":
		return Check{Name: "log path", State: StateFail, Detail: "config directory could not be resolved", Hint: "set [log_file] explicitly, or fix the config directory"}
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil {
		return Check{Name: "log path", State: StateWarn, Detail: fmt.Sprintf("%s does not exist yet", dir), Hint: "created automatically on the first log write; run any forgectl command to confirm"}
	}
	return Check{Name: "log path", State: StateOK, Detail: path}
}

// checkBinary reports whether name resolves on PATH via d.LookPath — the
// same seam internal/tmux and internal/ghostty already use for their own
// presence checks, reused here rather than duplicated.
func checkBinary(d Deps, name, hint string) Check {
	if p, err := d.LookPath(name); err == nil {
		return Check{Name: name, State: StateOK, Detail: p}
	}
	return Check{Name: name, State: StateWarn, Detail: name + " not found on PATH", Hint: hint}
}

// checkMdroll reports whether the optional mdroll terminal reader resolves on
// PATH. It is shaped like checkBinary, but a missing mdroll is StateSkip rather
// than StateWarn: `forgectl docs read` falls back to the HTML reader without
// it, so a machine that never installs it has nothing to fix.
//
// A hit only through a relative PATH entry (exec.ErrDot) is reported as its
// own case: `docs read` refuses to run it, so reporting it as OK would be
// wrong, and reporting it as absent would send the operator to install a
// binary they already have.
func checkMdroll(d Deps) Check {
	p, err := d.LookPath("mdroll")
	if err == nil {
		return Check{Name: "mdroll", State: StateOK, Detail: p}
	}
	if errors.Is(err, osexec.ErrDot) {
		return Check{
			Name:   "mdroll",
			State:  StateSkip,
			Detail: "mdroll found only via a relative PATH entry; ignoring",
			Hint:   "put mdroll's directory on PATH as an absolute path",
		}
	}
	return Check{
		Name:   "mdroll",
		State:  StateSkip,
		Detail: "not found on PATH — optional; `forgectl docs read` falls back to the HTML reader",
		Hint:   "install from https://github.com/tokuhirom/mdroll to read docs in the terminal",
	}
}

// checkGh reports whether the gh CLI is authenticated to the configured
// [github] host, via `gh auth status --hostname <host>` (report-only — never
// mutates). Absence of gh itself is folded into the same check rather than a
// separate binary probe, since an unauthenticated or missing gh means the
// same thing to every forgectl verb that shells out to it (pr, projects,
// branch, review): none of them will work.
//
// The question is host-scoped, so it runs through githubauth.Runner (#413): an
// ambient GH_HOST cannot answer it for a host nobody configured, and on a
// non-default host the ambient token variables are removed, so doctor reports
// the hosts.yml credential the pinned inventory will actually use. A host that
// fails validation is reported categorically — the rejected config value is
// never rendered.
func checkGh(ctx context.Context, d Deps) Check {
	if _, err := d.LookPath("gh"); err != nil {
		return Check{Name: "gh", State: StateFail, Detail: "gh not found on PATH", Hint: "install with `brew install gh`"}
	}
	host, err := githubauth.ResolveHost(d.Cfg.Github.Host)
	if err != nil {
		return Check{Name: "gh", State: StateFail, Detail: "configured [github] host failed validation", Hint: "set [github] host to a lowercase dns name with no port or scheme, or remove it for github.com"}
	}
	if _, err := githubauth.Runner(d.Runner, host).Run(ctx, "gh", "auth", "status", "--hostname", host); err != nil {
		// Categorical (#658): err is gh's stderr, text the host and any gh
		// extension choose. SafeLine at the report bounds its runes, not
		// its content, so it goes to the log instead.
		slog.Warn("gh auth status failed.", "host", host, "error", err)
		return Check{Name: "gh", State: StateFail, Detail: "gh auth status failed for " + host, Hint: "run `gh auth login --hostname " + host + "`"}
	}
	return Check{Name: "gh", State: StateOK, Detail: "authenticated to " + host}
}

// checkSops reports the sops VERSION, not merely its presence.
//
// `env set --sops` depends on behaviour that is version-specific and measured
// rather than documented: the exit status for an unchanged file, the absence of
// a trailing newline from `--extract --output`, and the editor re-invocation
// loop on an unparseable document. A doctor line that said only "found" would
// leave the one fact a future debugging session needs out of the report.
//
// A missing sops is StateSkip rather than StateFail: it is needed only for
// `env set --sops`, and a machine that never writes an encrypted secret is not
// unhealthy for lacking it.
func checkSops(ctx context.Context, d Deps) Check {
	if _, err := d.LookPath("sops"); err != nil {
		return Check{
			Name:   "sops",
			State:  StateSkip,
			Detail: "not found on PATH — only needed for `forgectl env set --sops`",
			Hint:   "install with `brew install sops`",
		}
	}
	out, err := d.Runner.Run(ctx, "sops", "--version", "--disable-version-check")
	if err != nil {
		// Categorical (#716): err is sops's argv and stderr.
		slog.Warn("sops --version failed.", "error", err)
		return Check{Name: "sops", State: StateFail, Detail: "sops --version failed", Hint: "reinstall with `brew reinstall sops`"}
	}
	// The Detail carries the version number parsed out of the first line, never
	// the line itself: sops can append an update notice, and whatever else it
	// prints is the tool's text, not ours (#716).
	line, _, _ := strings.Cut(out, "\n")
	if vs := selfupdate.FindVersions(line); len(vs) > 0 {
		return Check{Name: "sops", State: StateOK, Detail: "sops " + vs[0]}
	}
	slog.Warn("sops --version printed no recognizable version.", "output", termsafe.SafeLineMax(redact.Stdout(line), 200))
	return Check{Name: "sops", State: StateOK, Detail: "sops present; version not recognized"}
}

// benchChecks folds bench.Status's hearth and chronicle components into doctor
// Checks, translating bench's own State vocabulary rather than re-probing
// anything itself.
// bench.StateNotConfigured maps to StateSkip: an unconfigured bench
// component is a valid choice (a machine with no local bench), not a
// failure.
func benchChecks(ctx context.Context, d Deps) []Check {
	report := bench.Status(ctx, d.Cfg, d.Runner, d.Prober)
	return []Check{
		fromBenchComponent(report.Hearth),
		fromBenchComponent(report.Chronicle),
	}
}

func fromBenchComponent(c bench.Component) Check {
	check := Check{Name: c.Name, Detail: c.Reason}
	switch c.State {
	case bench.StateOK:
		check.State = StateOK
	case bench.StateNotConfigured:
		check.State = StateSkip
	case bench.StateDegraded:
		check.State = StateWarn
		check.Hint = "see `forgectl bench status` for detail"
	default: // bench.StateUnavailable
		check.State = StateFail
		check.Hint = "see `forgectl bench status` for detail"
	}
	return check
}

// checkTrustStore reports whether the workflow-blessing trust store is
// present and verifies under the compiled-in anchor (internal/bless).
//
// A machine with neither anchor nor store (never set up blessed workflows) is
// StateSkip too, so doctor's exit code is not 1 on every fresh install.
// A genuinely ABSENT store (bless.ErrTrustStoreMissing) is StateSkip, not
// StateFail — trust/blessing is opt-in infrastructure for `workflow bless`
// users, not every forgectl install. Any OTHER TrustedStore error —
// bless.ErrTrustStoreInvalid (a corrupt sidecar, or a store signed by a key
// other than the anchor) or bless.ErrNoAnchor (the compiled-in anchor is
// missing, not root-owned, or group/world-writable) — is StateFail: these
// are exactly the conditions this check exists to catch, and reporting them
// as "-" (skip, Report.Healthy() still true) would answer the health
// question wrongly rather than not answering it — worse than no check at
// all on a security-relevant path.
//
// ErrTrustStoreMissing wraps ErrTrustStoreInvalid (see bless/verify.go), so
// the missing check MUST run first: errors.Is(err, ErrTrustStoreInvalid)
// alone would match the missing case too and collapse back to always-skip.
func checkTrustStore(d Deps) Check {
	store, err := d.TrustedStore()
	switch {
	case err == nil:
		return Check{Name: "trust store", State: StateOK, Detail: fmt.Sprintf("verified, %d enrolled key(s)", len(store.Keys))}
	case errors.Is(err, bless.ErrTrustStoreMissing):
		return Check{Name: "trust store", State: StateSkip, Detail: "trust store not found", Hint: "run `forgectl workflow bless` to enroll a signing key, if you use blessed workflows"}
	case errors.Is(err, bless.ErrNoAnchor) && errors.Is(err, fs.ErrNotExist) && trustStoreAbsent(d):
		// No anchor AND no store: blessed workflows were never set up here, so
		// there is nothing to verify (forgectl#635). Any other anchor failure
		// — present but not root-owned, group/world-writable, unparseable — or
		// a store that exists without its anchor falls through to fail.
		return Check{Name: "trust store", State: StateSkip, Detail: "blessed workflows not set up (no trust anchor, no trust store)", Hint: "run `forgectl workflow bless` to set up blessed workflows, if you use them"}
	default:
		// Categorical (#716): err renders key ids, paths and decoder text read
		// from the store and anchor files on disk. The sentinel names which
		// root of trust failed; the log keeps the rest.
		slog.Warn("Trust store failed to verify.", "error", err)
		detail := "trust store could not be read or verified"
		switch {
		case errors.Is(err, bless.ErrNoAnchor):
			detail = "trust anchor is missing or not root-owned"
		case errors.Is(err, bless.ErrTrustStoreInvalid):
			detail = "trust store failed to verify under the anchor"
		}
		return Check{Name: "trust store", State: StateFail, Detail: detail, Hint: "the trust store or its root of trust failed to verify — see `forgectl workflow trust list` and bless/verify.go's error taxonomy"}
	}
}

// trustStoreAbsent reports whether the trust store file is confirmed absent.
// Anything else, including an unresolvable path or a nil seam, is "not
// confirmed": the caller keeps its failure rather than guess.
func trustStoreAbsent(d Deps) bool {
	if d.TrustStorePath == nil {
		return false
	}
	path, err := d.TrustStorePath()
	if err != nil {
		return false
	}
	_, statErr := os.Lstat(filepath.Clean(path))
	return errors.Is(statErr, fs.ErrNotExist)
}

// checkResumeTasks is the tripwire for `forgectl resume`'s one version
// coupling with Claude Code.
//
// Task rescue works by writing snapshotted task bodies into
// ~/.claude/tasks/<session-id>/ before resuming, because that is the directory
// a resumed session reads. That is verified behavior, not a guarantee: if a
// future Claude Code stops keying per-session task directories on the session
// id, restore would write where nothing reads and rescue nothing — silently,
// since writing succeeds either way. This check compares what restore creates
// against the dialects actually present on disk, so the drift surfaces here
// rather than as tasks that quietly stop coming back.
func checkResumeTasks(d Deps) Check {
	const name = "resume tasks"
	if d.ResumePaths == nil {
		return Check{Name: name, State: StateSkip, Detail: "no session-record paths configured"}
	}
	paths, err := d.ResumePaths()
	if err != nil {
		return Check{Name: name, State: StateSkip, Detail: err.Error()}
	}
	// Capture wiring comes first, because it is the failure that actually
	// costs data. `resume snapshot` is deliberately not auto-installed and
	// always exits 0 (it runs on a Stop hook and must never fail a turn), so
	// a hook that is missing, misspelled, or wired into the wrong settings
	// file is INDISTINGUISHABLE from one that works — until a session exits
	// and its tasks are gone. Live sessions with an empty store is the one
	// machine-detectable signature of that, so it is reported here rather
	// than discovered by losing something.
	if live, store := resume.CaptureState(paths); live > 0 && store == 0 {
		return Check{
			Name: name, State: StateWarn,
			Detail: fmt.Sprintf("%d live session(s) but no snapshots stored", live),
			Hint:   "`forgectl resume` cannot restore tasks for a session it never captured — merge the Stop hook from `forgectl resume --help` into the \"hooks\" object of ~/.claude/settings.json",
		}
	}

	drift := resume.DriftCheck(paths)
	switch {
	case !drift.Checked:
		return Check{Name: name, State: StateSkip, Detail: "no task directories on disk yet"}
	case drift.Drifted():
		return Check{
			Name: name, State: StateWarn,
			// Quoted, not %s: drift.Dir is a raw directory name off disk, where
			// every byte but '/' and NUL is legal. Quoting at construction
			// preserves the directory boundaries in both human output (which
			// also crosses SafeLine) and JSON output (which preserves values
			// while escaping its syntax), and QuoteArgMax bounds its length
			// (#716). Restores and Newest are resume's fixed dialect names.
			Detail: fmt.Sprintf("restore writes the %q dialect, but every task directory on disk is %q (newest: %s)", drift.Restores, drift.Newest, termsafe.QuoteArgMax(drift.Dir, 0)),
			Hint:   "Claude Code appears to have changed how it names task directories — `forgectl resume` would restore tasks where nothing reads them; please file this at github.com/cameronsjo/forgectl",
		}
	default:
		return Check{Name: name, State: StateOK, Detail: fmt.Sprintf("restore target dialect %q is present on disk", drift.Restores)}
	}
}

// checkForgectlVersion reports the Homebrew tap's reachability and forgectl's
// own currency in one probe (internal/selfupdate.CheckOutdated) — a
// successful call proves both the tap resolves and confirms whether a newer
// cask is available; a source build has no cask to compare against, so it
// reports StateSkip instead of attempting the brew call at all.
func checkForgectlVersion(ctx context.Context, d Deps) Check {
	if selfupdate.IsSourceBuild() {
		return Check{Name: "forgectl version", State: StateSkip, Detail: "source build — not tracked against the Homebrew tap"}
	}
	if _, err := d.LookPath("brew"); err != nil {
		return Check{Name: "forgectl version", State: StateWarn, Detail: "brew not found on PATH", Hint: "install Homebrew to enable `forgectl upgrade`"}
	}
	outdated, detail, err := selfupdate.CheckOutdated(ctx, d.Runner)
	if err != nil {
		// Categorical (#716): err is brew's argv and stderr, which relays
		// what the tap's server and git transport send.
		slog.Warn("brew outdated failed.", "error", err)
		return Check{Name: "forgectl version", State: StateWarn, Detail: "brew outdated failed", Hint: "check network access to the Homebrew tap"}
	}
	if outdated {
		return Check{Name: "forgectl version", State: StateWarn, Detail: selfupdate.OutdatedDetail(detail), Hint: "run `forgectl upgrade`"}
	}
	return Check{Name: "forgectl version", State: StateOK, Detail: "up to date"}
}
