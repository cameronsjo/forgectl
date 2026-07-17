package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/config"
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
