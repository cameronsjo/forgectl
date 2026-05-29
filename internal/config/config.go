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

	"github.com/BurntSushi/toml"
)

// Config holds user-settable defaults. Fields map directly to config.toml keys.
//
//	# ~/.config/forgectl/config.toml  (macOS: ~/Library/Application Support/forgectl/config.toml)
//	no_icons  = false
//	log_level = "off"   # off | debug | info | warn | error
//	log_file  = ""      # empty = auto; "-" = stderr
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
//   - ""  → auto: os.UserStateDir()/forgectl/forgectl.log
//   - "-" → os.Stderr (not closed)
//   - anything else → the literal path
//
// Falls back to (os.Stderr, nopCloser{}) on any setup error.
func openLogWriter(logFile string) (io.Writer, io.Closer) {
	if logFile == "-" {
		return os.Stderr, nopCloser{}
	}
	path := logFile
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return os.Stderr, nopCloser{}
		}
		path = filepath.Join(dir, "forgectl", "forgectl.log")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return os.Stderr, nopCloser{}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return os.Stderr, nopCloser{}
	}
	return f, f
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
