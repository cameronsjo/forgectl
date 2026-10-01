// Package gitleaks drives the optional gitleaks pass of `forgectl audit
// secrets` (forgectl#14, lane 2) and resolves the binary for it and for
// doctor's row.
//
// gitleaks runs over a working tree the operator did not write, so every
// knob a repo or the environment could turn is pinned here:
//
//   - the binary is an absolute path from PATH, never one PATH found through
//     a relative entry (exec.ErrDot) and never one inside the scan root,
//     which a clone could have planted;
//   - only `gitleaks dir` runs, never `gitleaks git`, which would run git
//     unhardened (#976), and never with --follow-symlinks;
//   - forgectl passes its own --config ([extend] useDefault = true), which
//     gitleaks reads ahead of a scanned repo's .gitleaks.toml, and removes
//     GITLEAKS_CONFIG and GITLEAKS_CONFIG_TOML from the environment;
//   - --gitleaks-ignore-path points at forgectl's private temp dir, so the
//     .gitleaksignore of whatever directory forgectl runs from is not read.
//     A scanned repo's own .gitleaksignore is still read (gitleaks loads it
//     unconditionally), which the native scan reports as scanner config;
//   - --redact, and a report decoded through a struct that holds only the
//     rule, file, line and fingerprint, so no secret text is ever decoded;
//   - a deadline over the whole pass, with the process group killed on it.
//
// Errors leave this package as categories (failed, timed out), never as a
// CommandError's text.
package gitleaks

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/selfupdate"
)

// Name is the executable resolved through PATH.
const Name = "gitleaks"

// MinVersion is the oldest gitleaks forgectl runs: v8.19.0 introduced the
// `dir` subcommand (its cmd/directory.go first appears at that tag) and has
// every flag Args passes.
const MinVersion = "8.19.0"

// versionTimeout bounds `gitleaks version`.
const versionTimeout = 10 * time.Second

// Binary states.
const (
	// StateAvailable: an absolute, out-of-root gitleaks at MinVersion or later.
	StateAvailable = "available"
	// StateAbsent: no gitleaks on PATH.
	StateAbsent = "absent"
	// StateRefused: a gitleaks forgectl will not run (see Binary.Reason).
	StateRefused = "refused"
	// StateTooOld: older than MinVersion, or a version forgectl cannot read.
	StateTooOld = "too_old"
	// StateVersionFailed: `gitleaks version` failed.
	StateVersionFailed = "version_failed"
)

// Refusal reasons.
const (
	// ReasonRelativePath: PATH found it only through a relative entry.
	ReasonRelativePath = "relative_path"
	// ReasonUnderScanRoot: it sits inside the projects root, where a cloned
	// repo could have put it.
	ReasonUnderScanRoot = "under_scan_root"
)

// Runner is the part of internal/exec's Runner the package needs.
type Runner interface {
	RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error)
}

// unsetEnv is removed from every gitleaks child's environment: either would
// replace forgectl's --config if --config were ever dropped, and neither is
// the operator's to aim at a scan forgectl runs.
var unsetEnv = []string{"GITLEAKS_CONFIG", "GITLEAKS_CONFIG_TOML"}

// UnsetEnv returns the variables removed from every gitleaks child's
// environment. The slice is fresh on every call.
func UnsetEnv() []string { return append([]string(nil), unsetEnv...) }

// Binary is a resolved gitleaks.
type Binary struct {
	State string
	// Path is the absolute path PATH resolved, when one was found.
	Path string
	// Version is the version `gitleaks version` reported, "" when unread.
	Version string
	// Reason says why a StateRefused binary was refused.
	Reason string
}

// Resolve finds gitleaks once, through lookPath, refuses one it must not
// run, and reads its version. scanRoot is the projects root ("" skips the
// under-root check). It never errors: what it found is the Binary's State.
func Resolve(ctx context.Context, lookPath func(string) (string, error), r Runner, scanRoot string) Binary {
	p, err := lookPath(Name)
	if errors.Is(err, exec.ErrDot) {
		return Binary{State: StateRefused, Reason: ReasonRelativePath}
	}
	if err != nil || p == "" {
		return Binary{State: StateAbsent}
	}
	if !filepath.IsAbs(p) {
		return Binary{State: StateRefused, Path: p, Reason: ReasonRelativePath}
	}
	if scanRoot != "" && underRoot(p, scanRoot) {
		return Binary{State: StateRefused, Path: p, Reason: ReasonUnderScanRoot}
	}
	vctx, cancel := context.WithTimeout(fexec.WithProcessGroup(ctx), versionTimeout)
	defer cancel()
	out, err := r.RunWithEnvFiltered(vctx, nil, UnsetEnv(), p, "version")
	if err != nil {
		return Binary{State: StateVersionFailed, Path: p}
	}
	line, _, _ := strings.Cut(out, "\n")
	vs := selfupdate.FindVersions(line)
	if len(vs) == 0 {
		return Binary{State: StateTooOld, Path: p}
	}
	if !AtLeast(vs[0], MinVersion) {
		return Binary{State: StateTooOld, Path: p, Version: vs[0]}
	}
	return Binary{State: StateAvailable, Path: p, Version: vs[0]}
}

// underRoot reports whether bin lies inside root, comparing both as given
// and with symlinks resolved, so neither a symlinked root nor a PATH entry
// symlinked into the root hides it. A binary whose links cannot be resolved
// cannot be judged, so it counts as inside.
func underRoot(bin, root string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return true
	}
	if within(bin, absRoot) {
		return true
	}
	realBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return true
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		realRoot = absRoot // a root that does not exist yet holds nothing
	}
	return within(realBin, realRoot) || within(realBin, absRoot)
}

// within reports whether p is root or below it, lexically.
func within(p, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// AtLeast reports whether version v is floor or later, comparing the
// numeric release parts only (a pre-release of floor counts as floor). An
// unparseable v is never at least anything.
func AtLeast(v, floor string) bool {
	a, ok := releaseParts(v)
	if !ok {
		return false
	}
	b, _ := releaseParts(floor)
	for i := range b {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

func releaseParts(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+_"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return out, false
	}
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
