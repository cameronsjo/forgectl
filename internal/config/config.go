// Package config loads persistent user preferences for forgectl and wires the
// global slog logger. All public functions have safe zero-value defaults so a
// missing config file is never an error.
package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// logKeepDays is how many daily log files are retained before pruning.
const logKeepDays = 7

// Config holds user-settable defaults. Fields map directly to config.toml keys.
//
//	# ~/.config/forgectl/config.toml  (macOS: ~/Library/Application Support/forgectl/config.toml)
//	no_icons  = false
//	log_level = "off"   # off | debug | info | warn | error
//	log_file  = ""      # empty = auto (daily rotation, 7 days kept); "-" = stderr
//
//	[launch.defaults]    # per-project Claude Code launcher (forgectl launch)
//	model = "opus"
//	[[launch.project]]
//	match = "~/Projects/minute"
//	model = "sonnet"
type Config struct {
	NoIcons  bool           `toml:"no_icons"`
	LogLevel string         `toml:"log_level"`
	LogFile  string         `toml:"log_file"`
	Launch   LaunchConfig   `toml:"launch"`
	Workflow WorkflowConfig `toml:"workflow"`
}

// LaunchConfig is the [launch] section: base defaults plus directory-keyed
// project overrides for the `forgectl launch` command group. The resolution
// logic (longest-prefix match, merge) lives in internal/launch — this package
// owns only the on-disk schema.
type LaunchConfig struct {
	Defaults LaunchDefaults  `toml:"defaults"`
	Projects []LaunchProject `toml:"project"`
}

// LaunchDefaults is [launch.defaults]: the base posture applied when no project
// matches (and the floor a matching project overrides). AllowDanger is a pointer
// so an explicit `false` is distinguishable from "unset".
type LaunchDefaults struct {
	Model          string            `toml:"model"`
	PermissionMode string            `toml:"permission_mode"`
	AllowDanger    *bool             `toml:"allow_danger"`
	Env            map[string]string `toml:"env"`
	AddDir         []string          `toml:"add_dir"`
	BinaryPath     string            `toml:"binary_path"` // explicit claude path; env FORGECTL_CLAUDE_BIN wins
}

// LaunchProject is one [[launch.project]] directory-keyed override block.
type LaunchProject struct {
	Match          string            `toml:"match"`
	Model          string            `toml:"model"`
	PermissionMode string            `toml:"permission_mode"`
	AllowDanger    *bool             `toml:"allow_danger"`
	Env            map[string]string `toml:"env"`
	AddDir         []string          `toml:"add_dir"`
}

// IsZero reports whether the [launch] section was absent or empty — the signal
// the launcher uses to fall back to a legacy claunch.conf.
func (lc LaunchConfig) IsZero() bool {
	return len(lc.Projects) == 0 && lc.Defaults.isZero()
}

// WorkflowConfig is the [workflow] section: the default strip-list the
// `strip` step falls back to when a workflow file's [[step]] omits `globs`.
// Its own built-in fallback is now sourced from quarantine.DefaultTargets
// (#20); this section remains the one config-driven override the DSL
// exposes.
type WorkflowConfig struct {
	StripGlobs []string `toml:"strip_globs"`
}

// IsZero reports whether the [workflow] section was absent or empty.
func (wc WorkflowConfig) IsZero() bool {
	return len(wc.StripGlobs) == 0
}

// isZero reports whether no [launch.defaults] value was set. LaunchDefaults
// holds maps/slices, so it is not comparable with == — check each field.
func (d LaunchDefaults) isZero() bool {
	return d.Model == "" && d.PermissionMode == "" && d.AllowDanger == nil &&
		len(d.Env) == 0 && len(d.AddDir) == 0 && d.BinaryPath == ""
}

// Load reads the config file. A missing file is not an error — it yields
// defaults. On a malformed file, Load logs a loud warning instead of silently
// returning a zero Config (which would also wipe the [launch] profiles); it
// returns whatever the decoder populated before erroring. Load runs before
// SetupLogger, so the warning reaches the default stderr handler regardless of
// the configured log_level.
func Load() Config {
	path, err := ConfigPath()
	if err != nil {
		return Config{}
	}
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to decode config file; using built-in defaults for unreadable sections.",
			"path", path, "error", err)
	}
	return cfg
}

// Validate decodes the config file and returns any parse error. A missing file
// is valid (built-in defaults). Used by `forgectl launch doctor` to surface a
// malformed config that Load() tolerated with a warning.
func Validate() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// SetupLogger configures the global slog default from cfg and returns a Closer
// that flushes/closes any opened log file. Logging setup is best-effort: if a
// log file cannot be created, the logger falls back to stderr. The returned
// Closer is always non-nil.
func SetupLogger(cfg Config) io.Closer {
	level, ok := parseLevel(cfg.LogLevel)
	if !ok {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return nopCloser{}
	}
	w, closer := openLogWriter(cfg.LogFile)
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
	return closer
}

// ResolvedLogPath returns the effective log file path for the given log_file
// config value — useful for display. Empty means auto (today's dated file),
// "-" means stderr, anything else is returned as-is.
func ResolvedLogPath(logFile string) string {
	switch logFile {
	case "-":
		return "stderr"
	case "":
		path, err := autoLogPath()
		if err != nil {
			return "(unavailable)"
		}
		return path
	default:
		return logFile
	}
}

// parseLevel maps a level name to slog.Level. Returns (level, true) for known
// names; (0, false) for "off" or anything unrecognised.
func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

// openLogWriter resolves cfg.LogFile to an (io.Writer, io.Closer) pair.
//   - ""  → auto: daily-rotated forgectl-YYYY-MM-DD.log in the config dir
//   - "-" → os.Stderr (not closed)
//   - anything else → the literal path (no rotation)
//
// Falls back to (os.Stderr, nopCloser{}) on any setup error.
func openLogWriter(logFile string) (io.Writer, io.Closer) {
	if logFile == "-" {
		return os.Stderr, nopCloser{}
	}

	var path string
	if logFile == "" {
		p, err := autoLogPath()
		if err != nil {
			return os.Stderr, nopCloser{}
		}
		path = p
	} else {
		path = logFile
	}
	logDir := filepath.Dir(path)

	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return os.Stderr, nopCloser{}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return os.Stderr, nopCloser{}
	}

	// Prune old daily log files (auto mode only; best-effort, non-fatal).
	if logFile == "" {
		pruneOldLogs(logDir)
	}

	return f, f
}

// configDir returns the OS config base directory for forgectl
// (os.UserConfigDir()/forgectl). Shared by ConfigPath and autoLogPath.
func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "forgectl"), nil
}

// autoLogPath returns today's log file path inside the config dir. No side effects.
func autoLogPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	name := "forgectl-" + time.Now().Format("2006-01-02") + ".log"
	return filepath.Join(dir, name), nil
}

// pruneOldLogs deletes forgectl-YYYY-MM-DD.log files in dir that are older
// than logKeepDays days. Errors are silently ignored — log pruning is never
// fatal.
func pruneOldLogs(dir string) {
	cutoff := time.Now().AddDate(0, 0, -logKeepDays)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "forgectl-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, "forgectl-"), ".log")
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			os.Remove(filepath.Join(dir, name)) //nolint:errcheck
		}
	}
}

// ConfigPath returns the expected config file path.
func ConfigPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// WorkflowsDir returns the user workflow directory `workflow run <name>`
// checks before the embedded built-ins: <os.UserConfigDir()>/forgectl/workflows
// (macOS: ~/Library/Application Support/forgectl/workflows; Linux:
// ~/.config/forgectl/workflows). It derives from the same configDir() base as
// every other forgectl path, so the two never drift.
func WorkflowsDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workflows"), nil
}

// LegacyLaunchPath returns the legacy claunch config location, honoring
// $XDG_CONFIG_HOME. Retained so `forgectl launch` keeps reading an existing
// ~/.config/claunch/claunch.conf until the user migrates the profiles into the
// [launch] section of config.toml (via `forgectl launch init`).
func LegacyLaunchPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "claunch", "claunch.conf"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "claunch", "claunch.conf"), nil
}

// LoadLegacyLaunch reads a legacy claunch.conf into a LaunchConfig — the same
// TOML shape as [launch] ([defaults] + [[project]]). It returns (cfg, true)
// only when a file was found and decoded; a missing file yields (zero, false),
// and a malformed file is logged and treated as absent.
func LoadLegacyLaunch() (LaunchConfig, bool) {
	path, err := LegacyLaunchPath()
	if err != nil {
		return LaunchConfig{}, false
	}
	var lc LaunchConfig
	if _, err := toml.DecodeFile(path, &lc); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("Failed to decode legacy claunch config; ignoring it.", "path", path, "error", err)
		}
		return LaunchConfig{}, false
	}
	return lc, true
}

// ValidateLegacyLaunch decodes the legacy claunch.conf and returns any parse
// error. A missing file is valid (nil). LoadLegacyLaunch collapses a malformed
// file to (zero, false), indistinguishable from absent — so `forgectl launch
// doctor` uses this to surface a broken legacy file instead of misreporting it
// as "no profiles configured".
func ValidateLegacyLaunch() error {
	path, err := LegacyLaunchPath()
	if err != nil {
		return err
	}
	var lc LaunchConfig
	if _, err := toml.DecodeFile(path, &lc); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// nopCloser is an io.Closer that does nothing — stdlib io.NopCloser wraps
// io.Reader, not a generic Closer, so we keep this small private type.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }
