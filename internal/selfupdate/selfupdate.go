// Package selfupdate is forgectl's self-update logic: detecting whether the
// running binary was installed via the Homebrew tap (the only supported
// upgrade path) or built from source, checking whether a newer cask version
// is available, and applying the upgrade by shelling out to brew — never by
// reimplementing brew's own download/checksum/install pipeline. It knows
// nothing of Cobra (the house pattern; mirrors internal/net, internal/clean).
//
// The footgun this package exists to prevent: `go build -o $(which forgectl) .`
// silently overwrites the brew-linked binary, desyncing it from Homebrew's
// own bookkeeping — the next `brew upgrade` either no-ops (brew thinks it's
// already current) or clobbers a locally-built binary the developer meant to
// keep. forgectl never offers a build-and-overwrite verb; Upgrade's only path
// is brew's own cask upgrade, which already owns its download, checksum
// verification (the cask's declared sha256), and atomic install — this
// package never touches the binary on disk directly.
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

// homebrewNoAutoUpdate disables Homebrew's own implicit tap-refresh-on-any-
// command behavior — mirrors internal/update's identical guard (steps.go) and
// the identical reasoning: without it, `brew outdated`/`brew upgrade` can
// silently fetch and mutate local tap state as a side effect neither `doctor`
// nor `upgrade` ever discloses.
var homebrewNoAutoUpdate = map[string]string{"HOMEBREW_NO_AUTO_UPDATE": "1"}

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
	out, err := run.RunWithEnv(ctx, homebrewNoAutoUpdate, "brew", "outdated", "--cask", CaskRef)
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

	updateOut, err := run.RunWithEnv(ctx, homebrewNoAutoUpdate, "brew", "update")
	if updateOut != "" {
		parts = append(parts, updateOut)
	}
	if err != nil {
		return strings.Join(parts, "\n\n"), fmt.Errorf("brew update: %w", err)
	}

	upgradeOut, err := run.RunWithEnv(ctx, homebrewNoAutoUpdate, "brew", "upgrade", "--cask", CaskRef)
	if upgradeOut != "" {
		parts = append(parts, upgradeOut)
	}
	if err != nil {
		return strings.Join(parts, "\n\n"), fmt.Errorf("brew upgrade --cask %s: %w", CaskRef, err)
	}
	return strings.Join(parts, "\n\n"), nil
}
