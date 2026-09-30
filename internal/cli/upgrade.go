package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	osexec "os/exec"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/selfupdate"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// upgradeModule declares the self-update extension (ADR-0005): no config
// section of its own.
var upgradeModule = module.Manifest{
	Name:      "upgrade",
	Tier:      module.TierExtension,
	ConfigKey: "",
	New:       newUpgradeCmd,
}

// upgradeLookPath is the PATH-resolution seam for `brew` — a package-level
// var so a test can stub it (mirrors confirmUpdateDestructive's seam
// pattern), rather than depending on whether brew is actually installed on
// the machine running `go test`.
var upgradeLookPath = osexec.LookPath

// newUpgradeCmd builds `forgectl upgrade` over the registry Deps.
func newUpgradeCmd(deps module.Deps) *cobra.Command {
	var checkOnly bool

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update forgectl safely via the Homebrew tap",
		Long: `upgrade updates forgectl the SAFE way: brew update (refresh the tap
index) followed by brew upgrade --cask for forgectl's own cask. Homebrew
owns the download, the checksum verification (the cask's declared sha256),
and the atomic install — this command never touches the binary on disk
itself.

KNOWN FOOTGUN this command exists to route around: ` + "`go build -o $(which forgectl) .`" + `
silently overwrites the brew-linked binary, desyncing it from Homebrew's own
bookkeeping — the next ` + "`brew upgrade`" + ` either no-ops (brew thinks it's
already current) or clobbers a build you meant to keep. forgectl has no
build-and-overwrite verb; this command's only path is brew's own cask
upgrade.

  forgectl upgrade            update via the Homebrew tap
  forgectl upgrade --check    report whether an update is available, no mutation

Running from a source build (go build/go run, not the released cask) has no
cask install to manage — upgrade WARNS and tells you what to run instead,
rather than refusing outright or guessing at what "upgrade" should mean for
a binary brew never installed.

Exit codes: 0 up to date (or a source build, warned), 1 the upgrade (or the
outdated check) failed.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd, deps, checkOnly)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "report whether an update is available, without applying it")
	return cmd
}

// runUpgrade is newUpgradeCmd's RunE body, split out so the source-build
// warning, the brew-presence check, and the check-only/apply branches are
// each a single readable step.
func runUpgrade(cmd *cobra.Command, deps module.Deps, checkOnly bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// The one ruling this command encodes: a source build WARNS, it never
	// refuses. There is no cask install to upgrade in place, and the running
	// binary may not even be the one `brew` would touch, so upgrade stops
	// here rather than guessing.
	if selfupdate.IsSourceBuild() {
		fmt.Fprintln(out, "forgectl was built from source (not installed via the Homebrew tap) — a self-update can't manage this.")
		fmt.Fprintln(out, "To update your source checkout: git pull && go build -o $(go env GOPATH)/bin/forgectl .")
		fmt.Fprintln(out, "To switch to the released version instead: brew install cameronsjo/tap/forgectl")
		return nil
	}

	if _, err := upgradeLookPath("brew"); err != nil {
		return WithExitCode(fmt.Errorf("brew not found on PATH — forgectl ships via the Homebrew tap; install Homebrew (https://brew.sh), or reinstall manually: %w", err), 1)
	}

	if checkOnly {
		return runUpgradeCheck(ctx, deps, out)
	}

	return runUpgradeApply(ctx, deps, out)
}

// runUpgradeApply is the applying path. Like --check (#738), it never renders
// brew's text (#761): brew's stdout relays what the tap's server and git
// transport send, and the CommandError carries brew's argv and stderr. The
// progress and the outcome are fixed text; brew's output goes to the debug
// log, whose text handler quotes it, and the CommandError stays on the chain
// for errors.As.
func runUpgradeApply(ctx context.Context, deps module.Deps, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "Refreshing the Homebrew tap and upgrading "+selfupdate.CaskRef+"…")
	upgradeOut, err := selfupdate.Upgrade(ctx, deps.Runner)
	if upgradeOut != "" {
		slog.Debug("brew output.", "output", upgradeOut)
	}
	if err != nil {
		slog.Warn("brew upgrade failed.", "error", err)
		return WithExitCode(termsafe.Categorical(upgradeFailure(ctx, err), err), 1)
	}
	if from, to, ok := selfupdate.UpgradedVersions(upgradeOut); ok {
		_, _ = fmt.Fprintf(out, "forgectl upgraded %s → %s — restart your shell (or open a new one) to pick up the new binary.\n", from, to)
		return nil
	}
	_, _ = fmt.Fprintln(out, "forgectl upgraded — restart your shell (or open a new one) to pick up the new binary.")
	return nil
}

// upgradeFailure words a failed apply from fixed text, by cause. The
// interrupt arm is defensive. The binary installs no signal context
// (main.go, forgectl#788), so a terminal Ctrl-C ends forgectl under Go's
// default disposition before this runs. The arm is reached only when ctx was
// cancelled some other way, a caller's deadline or a test. It comes first
// because os/exec reports a brew killed by that cancellation as a signal
// exit, not as context.Canceled, so the context itself is consulted too, and
// a cancellation must not read as a network fault.
func upgradeFailure(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return "upgrade: interrupted before brew finished; run `forgectl --version` to see what is installed"
	case errors.Is(err, selfupdate.ErrTapUpdate):
		return "upgrade: brew update failed; check network access to the Homebrew tap"
	default:
		return "upgrade: brew upgrade --cask failed; the installed forgectl may be unchanged; run `forgectl --version`"
	}
}

// runUpgradeCheck reports whether an upgrade is available, without applying
// one — `upgrade --check`'s body. Never mutates: it's the same
// selfupdate.CheckOutdated call `doctor`'s "forgectl version" check makes,
// and it words the result the same way (#738): the error is categorical,
// because it carries brew's argv and stderr, which relay what the tap's
// server and git transport send; the detail is rebuilt from the version
// tokens in brew's output, never from its text.
func runUpgradeCheck(ctx context.Context, deps module.Deps, out io.Writer) error {
	outdated, detail, err := selfupdate.CheckOutdated(ctx, deps.Runner)
	if err != nil {
		slog.Warn("brew outdated failed.", "error", err)
		return WithExitCode(termsafe.Categorical("check: brew outdated failed; check network access to the Homebrew tap", err), 1)
	}
	if outdated {
		_, _ = fmt.Fprintf(out, "update available: %s\n", selfupdate.OutdatedDetail(detail))
		return nil
	}
	fmt.Fprintln(out, "forgectl is up to date.")
	return nil
}
