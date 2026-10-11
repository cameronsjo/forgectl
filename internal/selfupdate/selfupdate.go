// Package selfupdate is forgectl's self-update logic: detecting whether the
// running binary was installed via the Homebrew tap (the only supported
// upgrade path) or built from source, checking whether a newer cask version
// is available, and applying the upgrade by shelling out to brew — never by
// reimplementing brew's own download/checksum/install pipeline. It knows
// nothing of Cobra (the house pattern; mirrors internal/net, internal/clean).
//
// The known footgun this package exists to prevent, and Upgrade's only path
// around it, are spelled out in full in `forgectl upgrade`'s own --help text
// (internal/cli/upgrade.go's Long string) — the user-facing explanation, not
// duplicated here.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/redact"
)

// CaskRef is the fully-qualified Homebrew cask reference `doctor`/`upgrade`
// operate on. Fully-qualified so brew resolves it even when the tap isn't
// explicitly `brew tap`-ped yet (brew auto-taps on a qualified reference).
const CaskRef = "cameronsjo/tap/forgectl"

// homebrewSafeEnv pins every brew invocation this package makes:
// exec.HomebrewNoAutoUpdate() (the same definition internal/update's brewStep
// merges onto its own calls, so the two packages can never drift apart on
// that shape) plus three additional HOMEBREW_* variables that redirect
// where brew's artifact or tap comes from: HOMEBREW_ARTIFACT_DOMAIN
// (download mirror), HOMEBREW_CASK_OPTS, and HOMEBREW_BREW_GIT_REMOTE (the
// tap's git remote). `upgrade`/`doctor` shell to brew from whatever
// directory the operator happens to be in — an ambient value for any of
// these (e.g. from a direnv-managed .envrc in a repo the operator already
// approved) could redirect the download or the tap on the one command whose
// output is a new binary on $PATH.
//
// Pinning to "" rather than omitting the key: Homebrew's own Ruby reads each
// via `.presence`, which treats an empty string the same as unset, falling
// back to its built-in default — so this neutralizes an ambient override
// without needing to literally strip a key from the inherited environment
// (which RunWithEnv's map-based API has no way to express). Overriding by
// appending is reliable because os/exec.Cmd.Env documents duplicate keys
// resolving to the LAST occurrence in the slice — our override always sorts
// after the inherited os.Environ() copy RunWithEnv builds from.
//
// It returns a fresh map on every call, so no brew invocation shares mutable
// env state with another (forgectl#851).
func homebrewSafeEnv() map[string]string {
	env := exec.HomebrewNoAutoUpdate()
	env["HOMEBREW_ARTIFACT_DOMAIN"] = ""
	env["HOMEBREW_CASK_OPTS"] = ""
	env["HOMEBREW_BREW_GIT_REMOTE"] = ""
	return env
}

// ErrTapUpdate and ErrCaskUpgrade mark which step of Upgrade failed, so a
// caller can word the failure from fixed text (#761) without parsing brew's
// argv or stderr, which relay what the tap's server and git transport send.
// A *StepError wraps one of them alongside the step's own error; errors.As
// still reaches the underlying exec.CommandError.
var (
	ErrTapUpdate   = errors.New("brew update failed")
	ErrCaskUpgrade = errors.New("brew upgrade --cask failed")
)

// IsSourceBuild reports whether the running binary lacks release metadata.
// meta.Version stays "dev" only on a plain `go build`/`go run` — goreleaser's
// ldflags always inject a real version (see internal/meta and
// .goreleaser.yaml). A source build isn't something Upgrade can safely
// manage: there is no cask install to upgrade in place, and the running
// binary may not even be the one `brew` would touch. Callers warn rather
// than attempt anything (the owner ruling this package encodes).
func IsSourceBuild() bool {
	return meta.Version == "dev"
}

// CheckOutdated reports whether the Homebrew tap has a newer forgectl cask
// than what's currently installed, via `brew outdated --cask` — never
// mutates, safe to call any time. Empty output means up to date; non-empty is
// brew's own outdated line, returned verbatim as detail.
func CheckOutdated(ctx context.Context, run exec.Runner) (outdated bool, detail string, err error) {
	out, err := run.RunWithEnv(ctx, homebrewSafeEnv(), "brew", "outdated", "--cask", CaskRef)
	if err != nil {
		return false, "", fmt.Errorf("brew outdated --cask %s: %w", CaskRef, err)
	}
	out = strings.TrimSpace(out)
	return out != "", out, nil
}

// Result is what a successful Upgrade did.
type Result struct {
	// Output is brew's combined output across both steps. It is untrusted
	// text: callers redact and escape it before showing it (Tail does).
	Output string
	// AlreadyCurrent is true when `brew upgrade --cask` exited non-zero but
	// the cask is installed and nothing is outdated, so there was nothing to
	// do. Output still holds what brew printed.
	AlreadyCurrent bool
}

// StepError is a failed Upgrade step. Error() is the fixed sentinel text, never
// brew's own (#761); the fields carry what an operator needs to see why.
type StepError struct {
	// Step is the command that failed, as shown to the operator.
	Step string
	// Kind is ErrTapUpdate or ErrCaskUpgrade.
	Kind error
	// ExitCode is brew's exit status, or -1 when it never exited normally.
	ExitCode int
	// Output is what the step printed (stdout then stderr), unsanitized.
	Output string
	// Cause is a fixed-text diagnosis from a read-only follow-up probe, or "".
	Cause string
	// Err is the runner's error, a *exec.CommandError in production.
	Err error
}

func (e *StepError) Error() string { return e.Kind.Error() }

// Unwrap lets errors.Is reach the sentinel and errors.As the CommandError.
func (e *StepError) Unwrap() []error { return []error{e.Kind, e.Err} }

// caskToken is the unqualified cask name. `brew list --cask` does not accept
// the tap-qualified CaskRef for an installed cask on every brew version, so the
// installed-or-not probe uses the token.
var caskToken = path.Base(CaskRef)

// Upgrade applies the safe upgrade path: `brew update` (refresh the tap
// index) followed by `brew upgrade --cask` for forgectl's own cask. Both
// commands' download, checksum verification (the cask's declared sha256),
// and atomic install are Homebrew's own — Upgrade never touches the binary
// on disk itself, so there is no temp file, no rename, and nothing here that
// could leave a half-written executable behind; a failure at either step
// leaves the previously-installed binary exactly as it was.
//
// When stream is non-nil each step prints a "== <command>" header to it and
// then brew's output, redacted and escaped line by line, as brew produces it
// (a runner without live streaming prints it when the step ends). Output is
// captured either way. A failed step returns a *StepError.
func Upgrade(ctx context.Context, run exec.Runner, stream io.Writer) (Result, error) {
	var parts []string

	out, err := runStep(ctx, run, stream, "brew update", "update")
	if out != "" {
		parts = append(parts, out)
	}
	if err != nil {
		return Result{Output: strings.Join(parts, "\n\n")}, newStepError("brew update", ErrTapUpdate, out, err, "")
	}

	step := "brew upgrade --cask " + CaskRef
	out, err = runStep(ctx, run, stream, step, "upgrade", "--cask", CaskRef)
	if out != "" {
		parts = append(parts, out)
	}
	if err != nil {
		cause, current := diagnoseCaskFailure(ctx, run)
		if current {
			return Result{Output: strings.Join(parts, "\n\n"), AlreadyCurrent: true}, nil
		}
		return Result{Output: strings.Join(parts, "\n\n")}, newStepError(step, ErrCaskUpgrade, out, err, cause)
	}
	return Result{Output: strings.Join(parts, "\n\n")}, nil
}

// brewUpdateLockedLine is what `brew update` prints when another `brew update`
// holds its lock (forgectl#1175).
const brewUpdateLockedLine = "Another `brew update` process is already running"

// UpdateLocked reports whether err is a failed `brew update` step that failed
// because another `brew update` held brew's lock, not because of the network.
// It reads brew's output only to classify it; callers still word the failure
// from fixed text.
func UpdateLocked(err error) bool {
	var se *StepError
	return errors.As(err, &se) && errors.Is(se.Kind, ErrTapUpdate) && strings.Contains(se.Output, brewUpdateLockedLine)
}

func newStepError(step string, kind error, out string, err error, cause string) *StepError {
	code := -1
	var ce *exec.CommandError
	if errors.As(err, &ce) {
		code = ce.ExitCode
	}
	return &StepError{Step: step, Kind: kind, ExitCode: code, Output: out, Cause: cause, Err: err}
}

// runStep runs one brew step with the pinned environment and returns what it
// printed. On failure the output is rebuilt from the CommandError, because the
// Runner contract returns no stdout with an error.
func runStep(ctx context.Context, run exec.Runner, stream io.Writer, step string, args ...string) (string, error) {
	env := homebrewSafeEnv()
	if stream != nil {
		_, _ = fmt.Fprintf(stream, "== %s\n", step)
	}
	if sr, ok := run.(exec.EnvStreamingRunner); ok && stream != nil {
		capture := &captureBuffer{}
		live := newSafeWriter(stream)
		w := io.MultiWriter(capture, live)
		err := sr.RunStreamingWithEnv(ctx, env, w, w, "brew", args...)
		live.Flush()
		return strings.TrimRight(capture.String(), "\n"), err
	}
	out, err := run.RunWithEnv(ctx, env, "brew", args...)
	if err != nil {
		out = commandErrorOutput(err)
	}
	if stream != nil && out != "" {
		live := newSafeWriter(stream)
		_, _ = io.WriteString(live, out+"\n")
		live.Flush()
	}
	return out, err
}

// commandErrorOutput is what a failed command printed: stdout, then stderr.
func commandErrorOutput(err error) string {
	var ce *exec.CommandError
	if !errors.As(err, &ce) {
		return ""
	}
	return strings.Trim(strings.Join([]string{redact.Stdout(ce.Output), ce.Stderr}, "\n"), "\n")
}

// diagnoseCaskFailure runs two read-only probes after a failed `brew upgrade
// --cask`. current is true when the cask is installed and nothing is outdated,
// so the failure was only "nothing to do". cause is a fixed-text reason when
// the cask is not installed as a cask at all (a formula install, or a copy
// put in place by hand), and "" when the probes explain nothing.
func diagnoseCaskFailure(ctx context.Context, run exec.Runner) (cause string, current bool) {
	listed, err := run.RunWithEnv(ctx, homebrewSafeEnv(), "brew", "list", "--cask", "--versions", caskToken)
	if err != nil {
		var ce *exec.CommandError
		if errors.As(err, &ce) && ce.ExitCode == 1 && strings.TrimSpace(ce.Output) == "" {
			return "forgectl is not installed as a Homebrew cask (installed as a formula, or by hand?), so brew upgrade --cask has nothing to upgrade. Reinstall with: brew install --cask " + CaskRef, false
		}
		return "", false
	}
	if strings.TrimSpace(listed) == "" {
		return "", false
	}
	outdated, _, err := CheckOutdated(ctx, run)
	if err != nil {
		return "", false
	}
	return "", !outdated
}
