package update

import (
	"context"
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

// brewStep: Check lists outdated formulae/casks (never mutates); Apply runs
// the standard three-command weekly refresh in sequence — update (refresh
// the formula index), upgrade (install newer versions), cleanup (reclaim
// disk). Destructive: cleanup ALSO removes old Cellar versions of every
// upgraded formula/cask, not just Homebrew's download cache (mirrors clean.go's
// same brew-cleanup disclosure).
func brewStep() Step {
	return Step{
		Name:        StepBrew,
		Destructive: true,
		Check: func(ctx context.Context, run exec.Runner) (string, error) {
			return run.Run(ctx, "brew", "outdated")
		},
		Apply: func(ctx context.Context, run exec.Runner) (string, error) {
			return runSequence(ctx, run,
				[]string{"brew", "update"},
				[]string{"brew", "upgrade"},
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
// Known quirk: `npm outdated` documents a non-zero exit whenever outdated
// packages ARE found — that is npm's normal signal for "here's the
// finding", not a broken check. Nothing here special-cases it: the shared
// exec.Runner contract already treats any non-zero exit as an error, so a
// routine "packages are outdated" day surfaces as this step's Result.Err on
// `update check`. That's a known, accepted false-positive-shaped signal
// (never a false negative) rather than something this step tries to
// distinguish from a genuinely broken npm — per-step isolation (the
// roster's whole point) means a noisy npm check never wedges brew/go/
// softwareupdate's siblings.
func npmStep() Step {
	return Step{
		Name:        StepNpm,
		Destructive: true,
		Check: func(ctx context.Context, run exec.Runner) (string, error) {
			return run.Run(ctx, "npm", "outdated", "-g")
		},
		Apply: func(ctx context.Context, run exec.Runner) (string, error) {
			return run.Run(ctx, "npm", "update", "-g")
		},
	}
}

// runSequence runs each argv in order, stopping at the first failure.
// Output from every command that ran (including the failing one) is joined
// with blank lines, so a partial-sequence failure (e.g. `brew update`
// succeeds, `brew upgrade` fails) still reports what already happened
// rather than only the failure.
func runSequence(ctx context.Context, run exec.Runner, argvs ...[]string) (string, error) {
	var parts []string
	for _, argv := range argvs {
		out, err := run.Run(ctx, argv[0], argv[1:]...)
		if out != "" {
			parts = append(parts, out)
		}
		if err != nil {
			return strings.Join(parts, "\n\n"), fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}
