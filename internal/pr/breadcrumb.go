package pr

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// Breadcrumb is the on-disk record of one clean-room review session, written
// under the forgectl session-state dir (config.PrSessionsDir). It is the sole
// bridge between `forgectl pr <ref>` and the later list/attach/teardown verbs
// — and it is HOSTILE INPUT on the way back in: a malicious breadcrumb must
// not be able to steer a `git -C <workspace>` at an arbitrary path, so
// LoadBreadcrumb validates both its LOCATION and its CONTENT before any caller
// touches Workspace.
type Breadcrumb struct {
	Workspace string    `json:"workspace"`
	Ref       string    `json:"ref"` // canonical "owner/repo#N"
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"createdAt"`
}

// breadcrumbFilename derives a stable, filesystem-safe name from the ref and
// creation time. Owner/repo are already constrained to [A-Za-z0-9._-] by
// ParseRef, so no separator collision or path segment can appear.
func breadcrumbFilename(ref Ref, createdAt time.Time) string {
	return fmt.Sprintf("%s-%s-%d-%d.json", ref.Owner, ref.Repo, ref.Number, createdAt.UnixNano())
}

// writeBreadcrumb writes bc into sessionsDir and returns the file path. The
// directory is created if absent (0700 — session state is private).
func writeBreadcrumb(sessionsDir string, ref Ref, bc Breadcrumb) (string, error) {
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		return "", fmt.Errorf("create pr sessions dir: %w", err)
	}
	data, err := json.MarshalIndent(bc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal breadcrumb: %w", err)
	}
	path := filepath.Join(sessionsDir, breadcrumbFilename(ref, bc.CreatedAt))
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write breadcrumb %s: %w", path, err)
	}
	slog.Debug("Wrote pr session breadcrumb.", "path", path, "ref", bc.Ref)
	return path, nil
}

// LoadBreadcrumb validates and loads the breadcrumb at path. It resolves the
// canonical session-state dir (config.PrSessionsDir) itself; see loadBreadcrumb
// for the injected-dir core used by the Client and tests.
func LoadBreadcrumb(path string) (Breadcrumb, error) {
	dir, err := config.PrSessionsDir()
	if err != nil {
		return Breadcrumb{}, fmt.Errorf("resolve pr sessions dir: %w", err)
	}
	return loadBreadcrumb(path, dir)
}

// loadBreadcrumb is the validation core. It enforces, IN ORDER and BEFORE any
// caller can act on Workspace:
//
//  1. LOCATION — path, after EvalSymlinks, must resolve to inside sessionsDir.
//     A path outside the dir, or a symlink inside it that points outside, is
//     rejected before the file is even read.
//  2. CONTENT/SCHEMA — the file must be valid JSON with all required fields,
//     and Workspace must be an EXISTING directory under the OS temp dir with
//     the "forgectl-" prefix (a real sandbox), so no arbitrary path can be
//     smuggled in for a later `git -C`.
func loadBreadcrumb(path, sessionsDir string) (Breadcrumb, error) {
	// (1) LOCATION — reject anything not inside the forgectl-owned dir first.
	if !sandbox.WithinWorkspace(sessionsDir, path) {
		slog.Error("Breadcrumb path escapes session-state dir; refusing.", "path", path, "sessionsDir", sessionsDir)
		return Breadcrumb{}, fmt.Errorf("breadcrumb %q is not inside the forgectl session-state dir", path)
	}

	// (2) CONTENT — only now read and decode.
	data, err := os.ReadFile(path) //nolint:gosec // path was location-validated above
	if err != nil {
		return Breadcrumb{}, fmt.Errorf("read breadcrumb %s: %w", path, err)
	}
	var bc Breadcrumb
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bc); err != nil {
		return Breadcrumb{}, fmt.Errorf("decode breadcrumb %s: %w", path, err)
	}
	if err := validateBreadcrumb(bc); err != nil {
		return Breadcrumb{}, fmt.Errorf("invalid breadcrumb %s: %w", path, err)
	}
	return bc, nil
}

// validateBreadcrumb enforces the content schema: required fields present, a
// re-parseable ref, and a Workspace that is a real forgectl sandbox (an
// existing dir under the OS temp dir with the "forgectl-" prefix).
func validateBreadcrumb(bc Breadcrumb) error {
	if bc.Workspace == "" {
		return fmt.Errorf("missing workspace")
	}
	if bc.Ref == "" {
		return fmt.Errorf("missing ref")
	}
	if _, err := ParseRef(bc.Ref); err != nil {
		return fmt.Errorf("malformed ref %q: %w", bc.Ref, err)
	}
	if bc.CreatedAt.IsZero() {
		return fmt.Errorf("missing createdAt")
	}
	if err := validateWorkspace(bc.Workspace); err != nil {
		return err
	}
	return nil
}

// validateWorkspace confirms workspace is an existing directory under the OS
// temp dir whose base name carries the forgectl sandbox prefix. This is the
// gate that stops a breadcrumb from pointing a later `git -C` at, say, / or
// $HOME.
func validateWorkspace(workspace string) error {
	if !filepath.IsAbs(workspace) {
		return fmt.Errorf("workspace %q must be an absolute path", workspace)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("workspace %q does not exist: %w", workspace, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", workspace)
	}
	if !sandbox.WithinWorkspace(osTempDir(), workspace) {
		return fmt.Errorf("workspace %q is not under the OS temp dir", workspace)
	}
	real := workspace
	if r, err := filepath.EvalSymlinks(workspace); err == nil {
		real = r
	}
	if !strings.HasPrefix(filepath.Base(real), tempPrefix) {
		return fmt.Errorf("workspace %q lacks the %q sandbox prefix", workspace, tempPrefix)
	}
	return nil
}
