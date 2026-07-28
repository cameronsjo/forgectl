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
	"os"
	osexec "os/exec"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/selfupdate"
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
	Prober       bench.Prober
}

// NewDeps wires Deps with production seams: os/exec.LookPath, the real
// bless.Verifier's trust-store read, and bench's real HTTP prober.
func NewDeps(cfg config.Config, runner exec.Runner) Deps {
	return Deps{
		Cfg:          cfg,
		Runner:       runner,
		LookPath:     osexec.LookPath,
		TrustedStore: bless.NewVerifier().TrustedStore,
		Prober:       bench.NewHTTPProber(),
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
	checks = append(checks, checkGh(ctx, d))
	checks = append(checks, benchChecks(ctx, d)...)
	checks = append(checks, checkTrustStore(d))
	checks = append(checks, checkForgectlVersion(ctx, d))

	return Report{Checks: checks}
}

// checkClaude reuses launch.ClaudePath — the exact resolution `forgectl
// launch doctor` already reports on (env override, configured binary_path,
// PATH) — rather than re-deriving PATH resolution here.
func checkClaude(d Deps) Check {
	if p, err := launch.ClaudePath(d.Cfg.Launch.Defaults); err == nil {
		return Check{Name: "claude", State: StateOK, Detail: p}
	} else {
		return Check{Name: "claude", State: StateFail, Detail: err.Error(), Hint: "install claude, or set [launch.defaults].binary_path / $FORGECTL_CLAUDE_BIN"}
	}
}

// checkConfig reuses config.Validate() — the exact parse `forgectl launch
// doctor` already surfaces — so a malformed config.toml fails the same way
// in both places.
func checkConfig(d Deps) Check {
	path, pathErr := config.ConfigPath()
	if err := config.Validate(); err != nil {
		return Check{Name: "config", State: StateFail, Detail: err.Error(), Hint: "fix the malformed config.toml (see the parse error above)"}
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

// checkGh reports whether the gh CLI is authenticated, via `gh auth status`
// (report-only — never mutates). Absence of gh itself is folded into the
// same check rather than a separate binary probe, since an unauthenticated
// or missing gh means the same thing to every forgectl verb that shells out
// to it (pr, projects, branch, review): none of them will work.
func checkGh(ctx context.Context, d Deps) Check {
	if _, err := d.LookPath("gh"); err != nil {
		return Check{Name: "gh", State: StateFail, Detail: "gh not found on PATH", Hint: "install with `brew install gh`"}
	}
	if _, err := d.Runner.Run(ctx, "gh", "auth", "status"); err != nil {
		return Check{Name: "gh", State: StateFail, Detail: err.Error(), Hint: "run `gh auth login`"}
	}
	return Check{Name: "gh", State: StateOK, Detail: "authenticated"}
}

// benchChecks folds bench.Status's three components (hearth, chronicle,
// flux — flux being the kanban board) into doctor Checks, translating
// bench's own State vocabulary rather than re-probing anything itself.
// bench.StateNotConfigured maps to StateSkip: an unconfigured bench
// component is a valid choice (a machine with no local bench), not a
// failure.
func benchChecks(ctx context.Context, d Deps) []Check {
	report := bench.Status(ctx, d.Cfg, d.Runner, d.Prober)
	return []Check{
		fromBenchComponent(report.Hearth),
		fromBenchComponent(report.Chronicle),
		fromBenchComponent(report.Flux),
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
		return Check{Name: "trust store", State: StateSkip, Detail: err.Error(), Hint: "run `forgectl workflow bless` to enroll a signing key, if you use blessed workflows"}
	default:
		return Check{Name: "trust store", State: StateFail, Detail: err.Error(), Hint: "the trust store or its root of trust failed to verify — see `forgectl workflow trust list` and bless/verify.go's error taxonomy"}
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
		return Check{Name: "forgectl version", State: StateWarn, Detail: err.Error(), Hint: "check network access to the Homebrew tap"}
	}
	if outdated {
		return Check{Name: "forgectl version", State: StateWarn, Detail: detail, Hint: "run `forgectl upgrade`"}
	}
	return Check{Name: "forgectl version", State: StateOK, Detail: "up to date"}
}
