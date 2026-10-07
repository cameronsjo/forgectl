package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/redact"
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
	var checkOnly, asJSON bool

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
  forgectl upgrade --json     machine-readable outcome; a failure carries the step and brew's last lines

On a terminal brew's output streams to stderr under a "== <command>" header
per step. Without one, a failure prints the last 20 lines of the failed step.
Either way brew's text is redacted and escaped first. If brew upgrade exits
non-zero but forgectl is installed as a cask and nothing is outdated, upgrade
reports "already up to date".

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
			return runUpgrade(cmd, deps, checkOnly, asJSON)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "report whether an update is available, without applying it")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"ok","already_current","from","to","error"} to stdout, with error {"step","exit_code","message","cause","output_tail"} on failure (brew's last lines, redacted and escaped); stdout stays valid JSON and brew's output does not stream; not valid with --check`)
	return cmd
}

// runUpgrade is newUpgradeCmd's RunE body, split out so the source-build
// warning, the brew-presence check, and the check-only/apply branches are
// each a single readable step.
func runUpgrade(cmd *cobra.Command, deps module.Deps, checkOnly, asJSON bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// The one ruling this command encodes: a source build WARNS, it never
	// refuses. There is no cask install to upgrade in place, and the running
	// binary may not even be the one `brew` would touch, so upgrade stops
	// here rather than guessing.
	if selfupdate.IsSourceBuild() {
		if asJSON {
			// stdout stays JSON-only; there is no upgrade document to emit.
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "forgectl was built from source (not installed via the Homebrew tap) — a self-update can't manage this.")
			return nil
		}
		fmt.Fprintln(out, "forgectl was built from source (not installed via the Homebrew tap) — a self-update can't manage this.")
		fmt.Fprintln(out, "To update your source checkout: git pull && go build -o $(go env GOPATH)/bin/forgectl .")
		fmt.Fprintln(out, "To switch to the released version instead: brew install cameronsjo/tap/forgectl")
		return nil
	}

	if _, err := upgradeLookPath("brew"); err != nil {
		return WithExitCode(fmt.Errorf("brew not found on PATH — forgectl ships via the Homebrew tap; install Homebrew (https://brew.sh), or reinstall manually: %w", err), exitFailed)
	}

	if asJSON && checkOnly {
		return WithExitCode(errors.New("upgrade: --json is not valid with --check"), exitUsage)
	}

	if checkOnly {
		return runUpgradeCheck(ctx, deps, out)
	}

	return runUpgradeApply(ctx, deps, out, cmd.ErrOrStderr(), asJSON)
}

// upgradeTailLines is how many of brew's last lines a failure shows when the
// output was not streamed live.
const upgradeTailLines = 20

// upgradeStderrIsTTY reports whether brew's live output can be shown. A
// package-level var so a test can stub it.
var upgradeStderrIsTTY = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }

// upgradeJSON is `upgrade --json`'s stdout document.
type upgradeJSON struct {
	OK             bool              `json:"ok"`
	AlreadyCurrent bool              `json:"already_current"`
	From           string            `json:"from,omitempty"`
	To             string            `json:"to,omitempty"`
	Error          *upgradeErrorJSON `json:"error"`
}

// upgradeErrorJSON names the failed step. Message is fixed text; OutputTail is
// brew's last lines, redacted and escaped.
type upgradeErrorJSON struct {
	Step       string `json:"step"`
	ExitCode   int    `json:"exit_code"`
	Message    string `json:"message"`
	Cause      string `json:"cause,omitempty"`
	OutputTail string `json:"output_tail"`
}

// runUpgradeApply is the applying path. On a terminal, brew's output streams to
// stderr as each step runs, redacted and escaped line by line (brew's stdout
// relays what the tap's server and git transport send, so it is untrusted,
// #761). Without a terminal, or under --json, nothing streams: a failure shows
// the last lines of the failed step's output the same way, and --json puts them
// in the error object. The outcome line itself stays fixed text, and the
// CommandError stays on the chain for errors.As.
func runUpgradeApply(ctx context.Context, deps module.Deps, out, errOut io.Writer, asJSON bool) error {
	var stream io.Writer
	if !asJSON {
		_, _ = fmt.Fprintln(out, "Refreshing the Homebrew tap and upgrading "+selfupdate.CaskRef+"…")
		if upgradeStderrIsTTY() {
			stream = errOut
		}
	}
	res, err := selfupdate.Upgrade(ctx, deps.Runner, stream)
	if res.Output != "" {
		slog.Debug("brew output.", "output", redact.Stdout(res.Output))
	}
	if err != nil {
		slog.Warn("brew upgrade failed.", "error", err)
		return upgradeFailed(ctx, out, errOut, err, asJSON, stream != nil)
	}
	from, to, named := selfupdate.UpgradedVersions(res.Output)
	if asJSON {
		doc := upgradeJSON{OK: true, AlreadyCurrent: res.AlreadyCurrent}
		if named {
			doc.From, doc.To = from, to
		}
		return writeJSON(out, doc)
	}
	switch {
	case res.AlreadyCurrent:
		_, _ = fmt.Fprintln(out, "forgectl is already up to date.")
	case named:
		_, _ = fmt.Fprintf(out, "forgectl upgraded %s → %s — restart your shell (or open a new one) to pick up the new binary.\n", from, to)
	default:
		_, _ = fmt.Fprintln(out, "forgectl upgraded — restart your shell (or open a new one) to pick up the new binary.")
	}
	return nil
}

// upgradeFailed reports a failed apply: the fixed-text error (exit 1), plus
// brew's output tail, on stderr or in --json's error object, unless it already
// streamed.
func upgradeFailed(ctx context.Context, out, errOut io.Writer, err error, asJSON, streamed bool) error {
	fixed := upgradeFailure(ctx, err)
	msg := fixed
	var step *selfupdate.StepError
	hasStep := errors.As(err, &step)
	if hasStep && step.Cause != "" {
		msg += "\n" + step.Cause
	}
	tail := ""
	if hasStep {
		tail = selfupdate.Tail(step.Output, upgradeTailLines)
	}
	switch {
	case asJSON:
		doc := upgradeJSON{Error: &upgradeErrorJSON{Message: fixed, OutputTail: tail}}
		if hasStep {
			doc.Error.Step, doc.Error.ExitCode, doc.Error.Cause = step.Step, step.ExitCode, step.Cause
		}
		if jerr := writeJSON(out, doc); jerr != nil {
			slog.Warn("upgrade --json write failed.", "error", jerr)
		}
	case tail != "" && !streamed:
		_, _ = fmt.Fprintf(errOut, "== last lines of %s\n%s\n", step.Step, tail)
	}
	return WithExitCode(termsafe.Categorical(msg, err), exitFailed)
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
		return WithExitCode(termsafe.Categorical("check: brew outdated failed; check network access to the Homebrew tap", err), exitFailed)
	}
	if outdated {
		_, _ = fmt.Fprintf(out, "update available: %s\n", selfupdate.OutdatedDetail(detail))
		return nil
	}
	fmt.Fprintln(out, "forgectl is up to date.")
	return nil
}
