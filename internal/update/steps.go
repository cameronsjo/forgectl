package update

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Step names — the vocabulary --only matches against and the roster.go
// completeness pin names.
const (
	StepBrew           = "brew"
	StepSoftwareUpdate = "softwareupdate"
	StepGo             = "go"
	StepNpm            = "npm"
)

// DefaultSteps is the built-in weekly-maintenance roster (forgectl#25):
// brew, softwareupdate, the go toolchain, and npm. Order here is the order
// `update run`/`update check` executes and reports in.
//
// pip is deliberately EXCLUDED: internal/pip already owns a `forgectl pip`
// command with different semantics (a comment-preserving index-url editor,
// not a package upgrader) — a SECOND, differently-behaved "pip" surface
// under `update` would be a naming trap (same word, two meanings), so this
// roster leaves pip out entirely rather than inventing a third.
func DefaultSteps() []Step {
	return []Step{
		brewStep(),
		softwareUpdateStep(),
		goStep(),
		npmStep(),
	}
}

// homebrewNoAutoUpdate disables Homebrew's own implicit "auto-update and
// refresh taps" behavior: several brew subcommands beyond `update` itself —
// `outdated`, `upgrade`, `install`, `list --versions`, … — silently trigger
// it on their own once the last auto-update is more than 24h stale. Without
// this, `brew outdated` (brewStep's Check, documented as "no mutation,
// always safe") can fetch and mutate local tap/formula-index state as a
// side effect the caller never asked for and `update check` never disclosed.
// Applied to every brew invocation in this step (Check and Apply alike) so
// the roster's behavior is deterministic regardless of how stale the
// ambient Homebrew auto-update timestamp happens to be.
var homebrewNoAutoUpdate = map[string]string{"HOMEBREW_NO_AUTO_UPDATE": "1"}

// brewStep: Check and Apply are both scoped to --formula — no cask is listed
// or upgraded here. One residue: `brew cleanup` takes no --formula/--cask
// switch, so it still prunes stale cask DOWNLOADS. Old-version deletion stays
// formula-only, which is what keeps the Cellar-rollback caveat below exact.
// Check lists outdated
// formulae (never mutates, HOMEBREW_NO_AUTO_UPDATE pinned — see above);
// Apply runs the standard three-command weekly refresh in sequence — update
// (refresh the formula index), upgrade --formula (install newer formula
// versions), cleanup (reclaim disk). Destructive: cleanup ALSO removes old
// Cellar versions of every upgraded formula, not just Homebrew's download
// cache (mirrors clean.go's same brew-cleanup disclosure) — and since this
// step runs upgrade immediately before cleanup in one pass, the very
// versions cleanup removes are the rollback path for the upgrade that just
// ran.
//
// upgrade is scoped to --formula (casks excluded) deliberately: a bare `brew
// upgrade` also upgrades casks, and a pkg-based cask upgrade can invoke sudo
// or a GUI installer. This step has no stdin wired and is meant to run
// unattended (cron, --yes), so a cask upgrade would either hang waiting for
// input nobody can supply, or fail opaquely with no tty — the same reasoning
// that keeps softwareupdate's OS installs manual and human-triggered. Check
// mirrors that same scope rather than reporting on what Apply can never
// act on: a bare `brew outdated` also lists casks, and an outdated cask
// would otherwise show up in every weekly check forever with nothing
// explaining why `update run --yes` never clears it. Cask upgrades stay a
// manual `brew upgrade --cask` outside this tool.
func brewStep() Step {
	return Step{
		Name:        StepBrew,
		Destructive: true,
		Check: func(ctx context.Context, run exec.Runner) (string, error) {
			return run.RunWithEnv(ctx, homebrewNoAutoUpdate, "brew", "outdated", "--formula")
		},
		Apply: func(ctx context.Context, run exec.Runner) (string, error) {
			return runSequence(ctx, run, homebrewNoAutoUpdate,
				[]string{"brew", "update"},
				[]string{"brew", "upgrade", "--formula"},
				[]string{"brew", "cleanup"},
			)
		},
	}
}

// softwareUpdateStep is CHECK-ONLY by design (forgectl#25's roster ruling):
// `softwareupdate -l` lists available macOS updates but Apply never installs
// anything — an OS install is disruptive enough (reboots, multi-hour
// installs) that it stays a manual, human-triggered action outside this
// tool. Destructive: false, and Apply is the identical listing function —
// `update run` always executes this step, unaffected by --yes.
func softwareUpdateStep() Step {
	list := func(ctx context.Context, run exec.Runner) (string, error) {
		return run.Run(ctx, "softwareupdate", "-l")
	}
	return Step{
		Name:        StepSoftwareUpdate,
		Destructive: false,
		Check:       list,
		Apply:       list,
	}
}

// goStep: Check reports the installed toolchain version (never mutates);
// Apply clears the build, module, and test caches — the standard "something
// in the Go cache is stale or corrupt" weekly reclaim, analogous to brew's
// cleanup. Destructive: a cleared module cache forces every project's next
// build to re-download its dependencies.
func goStep() Step {
	return Step{
		Name:        StepGo,
		Destructive: true,
		Check: func(ctx context.Context, run exec.Runner) (string, error) {
			return run.Run(ctx, "go", "version")
		},
		Apply: func(ctx context.Context, run exec.Runner) (string, error) {
			return run.Run(ctx, "go", "clean", "-cache", "-modcache", "-testcache")
		},
	}
}

// npmStep: Check lists outdated globally-installed packages (never
// mutates); Apply upgrades them in place. Destructive: `npm update -g`
// mutates every globally-installed package to its latest matching version.
//
// Known quirk: `npm outdated` documents a non-zero exit (status 1) whenever
// outdated packages ARE found — that is npm's normal signal for "here's the
// finding", not a broken check. Check special-cases exactly that shape: a
// real *exec.CommandError with ExitCode == 1 AND a non-empty Output is
// remapped to success, with the captured Output (the outdated-packages
// table — the actual contract of `update check`) surfacing as the step's
// Result instead of being reported as a failure.
//
// Both conditions are load-bearing, not just the exit code: exit 1 is ALSO
// npm's ordinary failure code (e.g. an unreachable registry — a dropped VPN,
// a wrong proxy, an npmjs outage — exits 1 with EMPTY stdout and the real
// error on stderr). Matching on ExitCode alone would remap that failure to
// a silent "ok npm" too: `update check` would exit 0 and the only
// diagnostic (stderr) would never surface, on a check that never actually
// ran. Requiring Output != "" is what keeps the remap narrow to the one
// shape npm actually produces for "found something to report" — any other
// exit code, an exit-1-with-empty-output failure, or a non-CommandError
// error (e.g. npm not on PATH at all) all still surface as a real error,
// carrying whatever stderr CommandError captured. Apply doesn't need the
// same treatment — `npm update -g` has no equivalent "exit nonzero to
// report a normal finding" contract.
func npmStep() Step {
	return Step{
		Name:        StepNpm,
		Destructive: true,
		Check: func(ctx context.Context, run exec.Runner) (string, error) {
			out, err := run.Run(ctx, "npm", "outdated", "-g")
			if err == nil {
				return out, nil
			}
			var cmdErr *exec.CommandError
			if errors.As(err, &cmdErr) && cmdErr.ExitCode == 1 && strings.TrimSpace(cmdErr.Output) != "" {
				return cmdErr.Output, nil
			}
			return out, err
		},
		Apply: func(ctx context.Context, run exec.Runner) (string, error) {
			return run.Run(ctx, "npm", "update", "-g")
		},
	}
}

// runSequence runs each argv in order (with env merged onto each
// invocation's environment), stopping at the first failure. Output from
// every command that ran — including the failing one — is joined with
// blank lines, so a partial-sequence failure (e.g. `brew update` succeeds,
// `brew upgrade` fails) still reports what already happened rather than
// only the failure. The failing command's own output doesn't come back on
// Runner.Run's own return value (that's always "" on error, by contract —
// see exec.OSRunner.Run) — it's recovered from the returned
// *exec.CommandError's Output field instead, which is where a failed
// command's captured stdout actually lives.
func runSequence(ctx context.Context, run exec.Runner, env map[string]string, argvs ...[]string) (string, error) {
	var parts []string
	for _, argv := range argvs {
		out, err := run.RunWithEnv(ctx, env, argv[0], argv[1:]...)
		if out != "" {
			parts = append(parts, out)
		}
		if err != nil {
			var cmdErr *exec.CommandError
			if errors.As(err, &cmdErr) && cmdErr.Output != "" {
				parts = append(parts, cmdErr.Output)
			}
			return strings.Join(parts, "\n\n"), fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}
