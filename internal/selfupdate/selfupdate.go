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
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
)

// CaskRef is the fully-qualified Homebrew cask reference `doctor`/`upgrade`
// operate on. Fully-qualified so brew resolves it even when the tap isn't
// explicitly `brew tap`-ped yet (brew auto-taps on a qualified reference).
const CaskRef = "cameronsjo/tap/forgectl"

// homebrewSafeEnv pins every brew invocation this package makes:
// exec.HomebrewNoAutoUpdate (the same definition internal/update's brewStep
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
var homebrewSafeEnv = map[string]string{
	"HOMEBREW_NO_AUTO_UPDATE":  exec.HomebrewNoAutoUpdate["HOMEBREW_NO_AUTO_UPDATE"],
	"HOMEBREW_ARTIFACT_DOMAIN": "",
	"HOMEBREW_CASK_OPTS":       "",
	"HOMEBREW_BREW_GIT_REMOTE": "",
}

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
	out, err := run.RunWithEnv(ctx, homebrewSafeEnv, "brew", "outdated", "--cask", CaskRef)
	if err != nil {
		return false, "", fmt.Errorf("brew outdated --cask %s: %w", CaskRef, err)
	}
	out = strings.TrimSpace(out)
	return out != "", out, nil
}

// Upgrade applies the safe upgrade path: `brew update` (refresh the tap
// index) followed by `brew upgrade --cask` for forgectl's own cask. Both
// commands' download, checksum verification (the cask's declared sha256),
// and atomic install are Homebrew's own — Upgrade never touches the binary
// on disk itself, so there is no temp file, no rename, and nothing here that
// could leave a half-written executable behind; a failure at either step
// leaves the previously-installed binary exactly as it was.
func Upgrade(ctx context.Context, run exec.Runner) (string, error) {
	var parts []string

	updateOut, err := run.RunWithEnv(ctx, homebrewSafeEnv, "brew", "update")
	if updateOut != "" {
		parts = append(parts, updateOut)
	}
	if err != nil {
		return strings.Join(parts, "\n\n"), fmt.Errorf("brew update: %w", err)
	}

	upgradeOut, err := run.RunWithEnv(ctx, homebrewSafeEnv, "brew", "upgrade", "--cask", CaskRef)
	if upgradeOut != "" {
		parts = append(parts, upgradeOut)
	}
	if err != nil {
		return strings.Join(parts, "\n\n"), fmt.Errorf("brew upgrade --cask %s: %w", CaskRef, err)
	}
	return strings.Join(parts, "\n\n"), nil
}
