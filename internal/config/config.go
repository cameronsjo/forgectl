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
type Config struct {
	NoIcons  bool   `toml:"no_icons"`
	LogLevel string `toml:"log_level"`
	LogFile  string `toml:"log_file"`
}

// Load reads the config file. If the file is missing or unreadable it returns
// defaults (no-icons=false, log-level=off) without error.
func Load() Config {
	path, err := ConfigPath()
	if err != nil {
		return Config{}
	}
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}
	}
	return cfg
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

	var logDir, path string
	if logFile == "" {
		p, err := autoLogPath()
		if err != nil {
			return os.Stderr, nopCloser{}
		}
		path = p
		logDir = filepath.Dir(path)
	} else {
		path = logFile
		logDir = filepath.Dir(path)
	}

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

// autoLogPath returns today's log file path inside the config dir. No side effects.
func autoLogPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	name := "forgectl-" + time.Now().Format("2006-01-02") + ".log"
	return filepath.Join(dir, "forgectl", name), nil
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
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "forgectl", "config.toml"), nil
}

// nopCloser is an io.Closer that does nothing — stdlib io.NopCloser wraps
// io.Reader, not a generic Closer, so we keep this small private type.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }
