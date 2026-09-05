package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/config"
	docspkg "github.com/cameronsjo/forgectl/internal/docs"
)

// cadenceFieldReportsEnv names the environment variable forgectl#93's
// default root set includes when set — the vault store cadence:
// writing-field-report writes to.
const cadenceFieldReportsEnv = "CADENCE_FIELD_REPORTS_DIR"

// resolveDocsRoots decides the root set `docs serve`/`docs list` indexes.
// Explicit positional args (directories or single markdown files) REPLACE
// the default set entirely — naming a path is a deliberate override, not an
// addition. With no args, the default set is cwd, ./docs (if it exists),
// and $CADENCE_FIELD_REPORTS_DIR (if set and exists), plus every extra root
// configured in [docs].roots (config.toml) — additive, since config-driven
// roots exist specifically to extend the defaults, not compete with them.
func resolveDocsRoots(args []string, cfg config.DocsConfig) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve cwd: %w", err)
	}
	roots := []string{cwd}

	if docsDir := filepath.Join(cwd, "docs"); isDir(docsDir) {
		roots = append(roots, docsDir)
	}
	if fr := os.Getenv(cadenceFieldReportsEnv); fr != "" && isDir(fr) {
		roots = append(roots, fr)
	}
	roots = append(roots, cfg.Roots...)

	return dedupPaths(roots), nil
}

// docsIndexOptions converts a [docs] section's root_kinds map into the
// docs.IndexOptions NewIndexWithOptions consumes. Keys pass through exactly
// as configured — no path normalization beyond what resolveDocsRoots already
// applies to the root set itself — so an entry must name a root the same way
// the caller's own roots argument does.
//
// An unknown value is a config error naming the offending key, its value,
// and the two allowed values: internal/config carries no field-level
// validation seam for [docs] (Validate/ValidatePath only surface TOML parse
// errors, never semantic ones), so this helper is where "docs" | "vault" is
// enforced.
func docsIndexOptions(cfg config.DocsConfig) (docspkg.IndexOptions, error) {
	if len(cfg.RootKinds) == 0 {
		return docspkg.IndexOptions{}, nil
	}
	kinds := make(map[string]docspkg.RootKind, len(cfg.RootKinds))
	for key, value := range cfg.RootKinds {
		switch value {
		case "docs":
			kinds[key] = docspkg.RootDocs
		case "vault":
			kinds[key] = docspkg.RootVault
		default:
			return docspkg.IndexOptions{}, fmt.Errorf("[docs].root_kinds[%q] = %q: must be %q or %q", key, value, "docs", "vault")
		}
	}
	return docspkg.IndexOptions{RootKinds: kinds}, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// dedupPaths removes duplicate roots (comparing by absolute path, so "." and
// an equivalent absolute cwd collapse to one entry) while preserving first-
// seen order.
func dedupPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		key := p
		if abs, err := filepath.Abs(p); err == nil {
			key = abs
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}
