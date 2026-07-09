package config

// Test Plan for internal/config/config.go
//
// parseLevel (Classification: pure logic)
//   [x] Happy: each known level name maps to the right slog.Level
//   [x] Boundary: case-insensitive + surrounding whitespace
//   [x] Unhappy: "off"/""/garbage → (0, false)
//
// ResolvedLogPath (Classification: pure logic)
//   [x] Happy: "-" → "stderr"; literal path returned as-is
//   [x] Happy: "" → dated auto path under the config dir
//
// pruneOldLogs (Classification: data transformer / I/O logic)
//   [x] Happy: deletes dated logs older than the retention window
//   [x] Boundary: keeps recent + today's logs
//   [x] Unhappy: ignores non-matching names, malformed dates, subdirs
//   [x] Unhappy: missing dir is a no-op (no panic)
//
// openLogWriter (Classification: I/O boundary)
//   [x] Happy: "-" → os.Stderr + nop closer
//   [x] Happy: literal path → file created, writer is the file
//   [x] Happy: "" → dated file created in config dir AND old logs pruned
//   [x] Unhappy: unmkdir-able path → falls back to os.Stderr
//
// Load / ConfigPath (Classification: configuration)
//   [x] Happy: ConfigPath → <configdir>/forgectl/config.toml
//   [x] Happy: valid TOML parses into Config
//   [x] Unhappy: missing file → zero-value defaults, no error
//   [x] Unhappy: malformed TOML → zero-value defaults, no error
//
// WorkflowsDir (Classification: configuration)
//   [x] Happy: WorkflowsDir → <configdir>/forgectl/workflows, same base as ConfigPath
//
// SetupLogger (Classification: I/O boundary)
//   [x] Happy: level "off" → non-nil closer, Close() == nil
//   [x] Happy: real level + file → log line lands in the file, closer closes it
//
// nopCloser (Classification: not worth a dedicated test)
//   Skipped: one-line method — exercised via openLogWriter("-") and SetupLogger("off").

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// redirectConfigDir points os.UserConfigDir() at a fresh temp dir for the test,
// on both darwin (HOME/Library/Application Support) and linux (XDG/HOME).
// Returns the resolved config dir.
func redirectConfigDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "") // force HOME-based path on linux
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	return dir
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in     string
		want   slog.Level
		wantOK bool
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		// case-insensitive + trimmed
		{"DEBUG", slog.LevelDebug, true},
		{"Info", slog.LevelInfo, true},
		{"  warn  ", slog.LevelWarn, true},
		{"ERROR", slog.LevelError, true},
		// disabled / unrecognised
		{"off", 0, false},
		{"", 0, false},
		{"  ", 0, false},
		{"trace", 0, false},
		{"bogus", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseLevel(tc.in)
			if ok != tc.wantOK {
				t.Errorf("parseLevel(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("parseLevel(%q) level = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolvedLogPath(t *testing.T) {
	t.Run("dash resolves to stderr", func(t *testing.T) {
		if got := ResolvedLogPath("-"); got != "stderr" {
			t.Errorf("ResolvedLogPath(%q) = %q, want %q", "-", got, "stderr")
		}
	})

	t.Run("literal path returned unchanged", func(t *testing.T) {
		lit := "/var/log/forgectl.log"
		if got := ResolvedLogPath(lit); got != lit {
			t.Errorf("ResolvedLogPath(%q) = %q, want unchanged", lit, got)
		}
	})

	t.Run("empty resolves to dated auto path", func(t *testing.T) {
		dir := redirectConfigDir(t)
		got := ResolvedLogPath("")

		wantDir := filepath.Join(dir, "forgectl")
		if filepath.Dir(got) != wantDir {
			t.Errorf("auto path dir = %q, want %q", filepath.Dir(got), wantDir)
		}
		base := filepath.Base(got)
		if !strings.HasPrefix(base, "forgectl-") || !strings.HasSuffix(base, ".log") {
			t.Errorf("auto path base = %q, want forgectl-<date>.log", base)
		}
		// The embedded date must parse as YYYY-MM-DD.
		dateStr := strings.TrimSuffix(strings.TrimPrefix(base, "forgectl-"), ".log")
		if _, err := time.Parse("2006-01-02", dateStr); err != nil {
			t.Errorf("auto path date %q does not parse: %v", dateStr, err)
		}
	})
}

func TestPruneOldLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	logName := func(d time.Time) string {
		return "forgectl-" + d.Format("2006-01-02") + ".log"
	}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	// Clearly older than the 7-day window → should be pruned.
	oldA := logName(now.AddDate(0, 0, -10))
	oldB := logName(now.AddDate(0, 0, -30))
	// Within the window → should survive.
	recent := logName(now.AddDate(0, 0, -2))
	today := logName(now)
	// Should never be touched, regardless of age.
	keepers := []string{"other.log", "forgectl-notadate.log", "forgectl-.log", "README.md"}

	write(oldA)
	write(oldB)
	write(recent)
	write(today)
	for _, k := range keepers {
		write(k)
	}
	// A subdir whose name matches the prefix pattern must be ignored.
	subdir := filepath.Join(dir, "forgectl-2000-01-01.log")
	if err := os.Mkdir(subdir, 0o700); err == nil {
		t.Cleanup(func() { _ = os.Remove(subdir) })
	}

	pruneOldLogs(dir)

	gone := []string{oldA, oldB}
	for _, name := range gone {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be pruned, but it survived (err=%v)", name, err)
		}
	}

	survivors := append([]string{recent, today}, keepers...)
	for _, name := range survivors {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to survive, but it's gone: %v", name, err)
		}
	}
}

func TestPruneOldLogs_MissingDirIsNoop(t *testing.T) {
	// Must not panic when the directory does not exist.
	pruneOldLogs(filepath.Join(t.TempDir(), "does-not-exist"))
}

func TestOpenLogWriter(t *testing.T) {
	t.Run("dash returns stderr and nop closer", func(t *testing.T) {
		w, c := openLogWriter("-")
		if w != os.Stderr {
			t.Errorf("writer = %v, want os.Stderr", w)
		}
		if err := c.Close(); err != nil {
			t.Errorf("nop closer Close() = %v, want nil", err)
		}
	})

	t.Run("literal path creates and opens the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "out.log")
		w, c := openLogWriter(path)
		defer c.Close()

		if w == os.Stderr {
			t.Fatal("writer fell back to stderr, want the literal file")
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("log file not created at %q: %v", path, err)
		}
	})

	t.Run("empty creates dated file and prunes old logs", func(t *testing.T) {
		dir := redirectConfigDir(t)
		logDir := filepath.Join(dir, "forgectl")
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			t.Fatalf("mkdir logDir: %v", err)
		}
		// Seed an ancient log that the open() should prune.
		old := filepath.Join(logDir, "forgectl-2000-01-01.log")
		if err := os.WriteFile(old, []byte("ancient"), 0o600); err != nil {
			t.Fatalf("seed old log: %v", err)
		}

		w, c := openLogWriter("")
		defer c.Close()

		if w == os.Stderr {
			t.Fatal("writer fell back to stderr, want the dated auto file")
		}
		// Today's dated file exists.
		today := filepath.Join(logDir, "forgectl-"+time.Now().Format("2006-01-02")+".log")
		if _, err := os.Stat(today); err != nil {
			t.Errorf("dated log not created at %q: %v", today, err)
		}
		// Ancient file pruned.
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Errorf("expected ancient log pruned, survived (err=%v)", err)
		}
	})

	t.Run("unmkdir-able path falls back to stderr", func(t *testing.T) {
		// Create a regular file, then demand a log path *under* it — MkdirAll
		// must fail because a parent component is not a directory.
		blocker := filepath.Join(t.TempDir(), "iamafile")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed blocker: %v", err)
		}
		w, c := openLogWriter(filepath.Join(blocker, "sub", "out.log"))
		defer c.Close()
		if w != os.Stderr {
			t.Errorf("writer = %v, want os.Stderr fallback", w)
		}
		if err := c.Close(); err != nil {
			t.Errorf("fallback closer Close() = %v, want nil", err)
		}
	})
}

func TestConfigPath(t *testing.T) {
	dir := redirectConfigDir(t)
	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	want := filepath.Join(dir, "forgectl", "config.toml")
	if got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestWorkflowsDir(t *testing.T) {
	dir := redirectConfigDir(t)
	got, err := WorkflowsDir()
	if err != nil {
		t.Fatalf("WorkflowsDir: %v", err)
	}
	want := filepath.Join(dir, "forgectl", "workflows")
	if got != want {
		t.Errorf("WorkflowsDir() = %q, want %q", got, want)
	}
}

func TestPrSessionsDir(t *testing.T) {
	dir := redirectConfigDir(t)
	got, err := PrSessionsDir()
	if err != nil {
		t.Fatalf("PrSessionsDir: %v", err)
	}
	want := filepath.Join(dir, "forgectl", "pr-sessions")
	if got != want {
		t.Errorf("PrSessionsDir() = %q, want %q", got, want)
	}
}

func TestLoad(t *testing.T) {
	t.Run("missing file returns defaults", func(t *testing.T) {
		redirectConfigDir(t) // empty temp config dir → no config.toml
		got := Load()
		if !reflect.DeepEqual(got, Config{}) {
			t.Errorf("Load() with no file = %+v, want zero-value Config", got)
		}
	})

	t.Run("valid TOML parses into Config", func(t *testing.T) {
		dir := redirectConfigDir(t)
		cfgDir := filepath.Join(dir, "forgectl")
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		body := "no_icons = true\nlog_level = \"debug\"\nlog_file = \"-\"\n"
		if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}

		got := Load()
		want := Config{NoIcons: true, LogLevel: "debug", LogFile: "-"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Load() = %+v, want %+v", got, want)
		}
	})

	t.Run("malformed TOML returns defaults without error", func(t *testing.T) {
		dir := redirectConfigDir(t)
		cfgDir := filepath.Join(dir, "forgectl")
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Not valid TOML.
		if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("this = = broken ["), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		got := Load()
		if !reflect.DeepEqual(got, Config{}) {
			t.Errorf("Load() with malformed file = %+v, want zero-value Config", got)
		}
	})
}

func TestSetupLogger(t *testing.T) {
	// SetupLogger mutates the global slog default — restore it afterward.
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	t.Run("off level returns a working nop closer", func(t *testing.T) {
		c := SetupLogger(Config{LogLevel: ""}) // "" → off
		if c == nil {
			t.Fatal("SetupLogger returned nil closer")
		}
		if err := c.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})

	t.Run("real level writes log lines to the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "forge.log")
		c := SetupLogger(Config{LogLevel: "debug", LogFile: path})
		if c == nil {
			t.Fatal("SetupLogger returned nil closer")
		}
		slog.Info("Successfully wired logger.", "marker", "polish-probe")
		if err := c.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read log file: %v", err)
		}
		if !strings.Contains(string(data), "polish-probe") {
			t.Errorf("log file missing expected marker; got:\n%s", data)
		}
	})
}
