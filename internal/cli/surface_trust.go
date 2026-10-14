package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// errUntrustedFolder refuses a surface launch whose directory Claude Code has
// never trusted for the account the session will run as (forgectl#1240).
// Without it the launch exits 0 and the session sits at the folder-trust
// dialog in a pane nobody may be watching, and the first sign is `surface
// ready` reporting the dialog. forgectl never writes trust itself: the dialog
// guards repo-supplied hooks, MCP servers and CLAUDE.md, so the operator
// answers it once, by hand.
var errUntrustedFolder = errors.New("launch: Claude Code has not trusted this folder")

// claudeTrustState is what the folder-trust check could tell.
type claudeTrustState int

const (
	// trustUnknown: the account's config could not be read or parsed, so the
	// check refuses nothing. A missing file is unknown too: Claude Code then
	// also runs its first-start onboarding, which the check does not model.
	trustUnknown claudeTrustState = iota
	trustTrusted
	trustUntrusted
)

// claudeGlobalConfigFile is the file Claude Code keeps an account's state in,
// folder trust included: <config dir>/.claude.json for a CLAUDE_CONFIG_DIR
// account, else ~/.claude.json. A legacy .config.json in the config home
// (CLAUDE_CONFIG_DIR, else ~/.claude) wins when it exists, as it does for
// Claude Code. "" when no home is known for the default account.
func claudeGlobalConfigFile(configDir, home string) string {
	configHome := configDir
	if configHome == "" {
		if home == "" {
			return ""
		}
		configHome = filepath.Join(home, ".claude")
	}
	if legacy := filepath.Join(configHome, ".config.json"); fileExists(legacy) {
		return legacy
	}
	if configDir != "" {
		return filepath.Join(configDir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

func fileExists(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // G703: the harness account's config home, from CLAUDE_CONFIG_DIR or the home directory, only stat'd
	return err == nil && info.Mode().IsRegular()
}

// claudeFolderTrust reports whether dir is trusted in the Claude Code config
// file. Trust is inherited: Claude Code skips the dialog for a directory under
// any trusted one. That was observed live on 2.1.289
// (docs/plans/2026-09-28-forgectl-herdr-coordinator.md) and read from
// 2.1.296's source, whose check walks the parents of the session's cwd and
// also accepts the git root's own entry. So dir's physical path and every
// parent of it count, and the spelling given counts as an exact key. That is the widest reading of the
// inheritance rule, chosen so the check never refuses a folder the harness
// would open without the dialog.
func claudeFolderTrust(configFile, dir string) claudeTrustState {
	if configFile == "" {
		return trustUnknown
	}
	data, err := os.ReadFile(configFile) //nolint:gosec // G304: the harness account's own config file, read only
	if err != nil {
		return trustUnknown
	}
	var doc struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return trustUnknown
	}
	trusted := func(path string) bool {
		return doc.Projects[path].HasTrustDialogAccepted || doc.Projects[filepath.ToSlash(path)].HasTrustDialogAccepted
	}
	// The spelling given counts only as an exact key: Claude Code sees the
	// physical path, so a trusted parent of a symlinked spelling
	// (/trusted/link -> /untrusted) says nothing about the folder.
	if trusted(filepath.Clean(dir)) {
		return trustTrusted
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		physical = filepath.Clean(dir)
	}
	for d := physical; ; d = filepath.Dir(d) {
		if trusted(d) {
			return trustTrusted
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return trustUntrusted
}

// checkClaudeFolderTrust refuses a claude launch into dir when the account
// whose config dir is configDir ("" for the default account) has not trusted
// it. It refuses only on a definite "untrusted": an unreadable config lets the
// launch go ahead as before.
func checkClaudeFolderTrust(dir, configDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		// No home: the default account's file cannot be found, so its trust is
		// unknown and nothing is refused. A config dir account needs no home.
		home = ""
	}
	file := claudeGlobalConfigFile(configDir, home)
	if claudeFolderTrust(file, dir) != trustUntrusted {
		return nil
	}
	run := "claude"
	if configDir != "" {
		run = "CLAUDE_CONFIG_DIR=" + quoteShellWord(configDir) + " claude"
	}
	return fmt.Errorf("%w: %s has no trusted entry, for it or a parent, in %s; trust it once by hand: cd %s && %s, accept the folder-trust dialog, then /exit",
		errUntrustedFolder, termsafe.QuotePath(dir), termsafe.QuotePath(file), quoteShellWord(dir), run)
}

// quoteShellWord is shellQuote for a command printed in an error, falling
// back to the escaped form when the text cannot be one shell word.
func quoteShellWord(s string) string {
	if q, err := shellQuote(s); err == nil {
		return q
	}
	return termsafe.QuotePathMax(s, termsafe.PathEchoMaxRunes)
}

// envValue is the last value of key in a KEY=value list, or "".
func envValue(env []string, key string) string {
	v := ""
	for _, e := range env {
		if k, val, ok := strings.Cut(e, "="); ok && k == key {
			v = val
		}
	}
	return v
}
