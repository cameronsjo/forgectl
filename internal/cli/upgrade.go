package cli

import (
	"context"
	"fmt"
	"io"
	osexec "os/exec"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/selfupdate"
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

	upgradeOut, err := selfupdate.Upgrade(ctx, deps.Runner)
	if upgradeOut != "" {
		fmt.Fprintln(out, upgradeOut)
	}
	if err != nil {
		return WithExitCode(fmt.Errorf("upgrade: %w", err), 1)
	}
	fmt.Fprintln(out, "forgectl upgraded — restart your shell (or open a new one) to pick up the new binary.")
	return nil
}

// runUpgradeCheck reports whether an upgrade is available, without applying
// one — `upgrade --check`'s body. Never mutates: it's the same
// selfupdate.CheckOutdated call `doctor`'s "forgectl version" check makes.
func runUpgradeCheck(ctx context.Context, deps module.Deps, out io.Writer) error {
	outdated, detail, err := selfupdate.CheckOutdated(ctx, deps.Runner)
	if err != nil {
		return WithExitCode(fmt.Errorf("check: %w", err), 1)
	}
	if outdated {
		fmt.Fprintf(out, "update available: %s\n", detail)
		return nil
	}
	fmt.Fprintln(out, "forgectl is up to date.")
	return nil
}
