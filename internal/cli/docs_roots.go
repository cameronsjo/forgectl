package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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

	cfg, err := expandDocsConfig(cfg, os.UserHomeDir)
	if err != nil {
		return nil, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve cwd: %w", termsafe.Error(err))
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

// expandDocsConfig expands a leading ~ in [docs].roots and the root_kinds
// keys to the home directory. The home is looked up (through userHome) only
// when some entry starts with ~ or ~/, so a config of absolute paths never
// depends on it. When such an entry exists and the lookup fails it returns an
// error rather than the literal ~/... path, which would resolve against the
// working directory and index the wrong tree; config's resolveDir fails
// closed the same way. Positional args are never expanded: the shell already
// did.
func expandDocsConfig(cfg config.DocsConfig, userHome func() (string, error)) (config.DocsConfig, error) {
	if !docsConfigUsesHome(cfg) {
		return cfg, nil
	}
	home, err := userHome()
	if err != nil {
		return config.DocsConfig{}, fmt.Errorf("[docs]: a path starts with ~ but the home directory cannot be resolved: %w", termsafe.Error(err))
	}
	return cfg.ExpandHome(home)
}

// docsConfigUsesHome reports whether any [docs].roots entry or root_kinds key
// is "~" or starts with "~/" — the only spellings ExpandHome rewrites.
func docsConfigUsesHome(cfg config.DocsConfig) bool {
	for _, r := range cfg.Roots {
		if r == "~" || strings.HasPrefix(r, "~/") {
			return true
		}
	}
	for k := range cfg.RootKinds {
		if k == "~" || strings.HasPrefix(k, "~/") {
			return true
		}
	}
	return false
}

// docsIndexOptions converts a [docs] section's root_kinds map into the
// docs.IndexOptions NewIndexWithOptions consumes, after config.DocsConfig's
// own Validate has rejected any value outside "docs" | "vault". Keys pass
// through as configured; internal/docs matches them to roots by absolute
// path, so a config file may spell a root relatively.
func docsIndexOptions(cfg config.DocsConfig) (docspkg.IndexOptions, error) {
	if err := cfg.Validate(); err != nil {
		return docspkg.IndexOptions{}, err
	}
	cfg, err := expandDocsConfig(cfg, os.UserHomeDir)
	if err != nil {
		return docspkg.IndexOptions{}, err
	}
	if len(cfg.RootKinds) == 0 {
		return docspkg.IndexOptions{}, nil
	}
	kinds := make(map[string]docspkg.RootKind, len(cfg.RootKinds))
	for key, value := range cfg.RootKinds {
		if value == config.RootKindVault {
			kinds[key] = docspkg.RootVault
		} else {
			kinds[key] = docspkg.RootDocs
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
