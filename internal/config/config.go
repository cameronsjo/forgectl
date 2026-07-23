// Package config loads persistent user preferences for forgectl and wires the
// global slog logger. All public functions have safe zero-value defaults so a
// missing config file is never an error.
package config

import (
	"errors"
	"fmt"
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
//
//	[net]                # forgectl net — cached internal-network reachability probe
//	probe_host = "1.1.1.1"
//	probe_port = 443
//	ttl_seconds = 60
//	timeout_ms = 1000
//	[bench]              # interop with the local bench (forgectl bench)
//	hearth_dir    = "~/Projects/hearth"      # else $HEARTH_DIR
//	chronicle_dir = "~/Projects/chronicle"   # else $CHRONICLE_DIR
//	otlp_endpoint = "http://localhost:16317" # hearth's frozen OTLP transport
//	otlp_protocol = "grpc"
//	telemetry     = false                    # opt-in: inject OTLP env into launches
//	[docker]             # forgectl docker — git-derived build/run/shell
//	default_platform = "linux/amd64" # --platform default when the flag is omitted
//	label_template    = ""           # extra "key=value" OCI label appended to every build
//	[clean]              # forgectl clean — dep/build-dir reclaim
//	default_root = "~/Projects"      # --root default when the flag is omitted
//	default_type = ""                # --type default: node|python|go|build, "" = all
//	[sessions]           # forgectl sessions — cross-machine operational mart ETL
//	dsn     = "postgres://user@host:5433/sessions_mart" # password via ~/.pgpass; env FORGECTL_SESSIONS_DSN wins
//	machine = ""                     # provenance label; default: short hostname
//	[review]             # forgectl review — cross-project work inventory
//	owners = ["cameronsjo"]          # gh search --owner scope; default cameronsjo
//	[docs]               # forgectl docs — local markdown reader
//	roots = ["~/Projects/notes"]     # extra root dirs indexed alongside cwd/./docs
//	addr  = "127.0.0.1:4712"         # --addr default when the flag is omitted
//	[preflight]          # forgectl preflight — plugin/catalog alignment
//	catalog_path = ""    # override auto-locate; direct path to the generated catalog.md
//	default_set  = []    # extra "plugin@marketplace" entries always folded into the core-tier target
type Config struct {
	NoIcons   bool            `toml:"no_icons"`
	LogLevel  string          `toml:"log_level"`
	LogFile   string          `toml:"log_file"`
	Launch    LaunchConfig    `toml:"launch"`
	Workflow  WorkflowConfig  `toml:"workflow"`
	Net       NetConfig       `toml:"net"`
	Bench     BenchConfig     `toml:"bench"`
	Docker    DockerConfig    `toml:"docker"`
	Clean     CleanConfig     `toml:"clean"`
	Sessions  SessionsConfig  `toml:"sessions"`
	Review    ReviewConfig    `toml:"review"`
	Docs      DocsConfig      `toml:"docs"`
	Preflight PreflightConfig `toml:"preflight"`
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

// NetConfig is the [net] section: the endpoint `forgectl net` probes for
// internal-network reachability, and how long a cached answer stays fresh.
// A zero value means "section absent" — internal/net's Client applies its own
// built-in defaults (probe_host 1.1.1.1, probe_port 443, ttl_seconds 60,
// timeout_ms 1000) for whichever fields are left unset.
type NetConfig struct {
	ProbeHost  string `toml:"probe_host"`
	ProbePort  int    `toml:"probe_port"`
	TTLSeconds int    `toml:"ttl_seconds"`
	TimeoutMs  int    `toml:"timeout_ms"`
}

// IsZero reports whether the [net] section was absent or empty.
func (nc NetConfig) IsZero() bool {
	return nc.ProbeHost == "" && nc.ProbePort == 0 && nc.TTLSeconds == 0 && nc.TimeoutMs == 0
}

// DockerConfig is the [docker] section: build-time defaults for `forgectl
// docker build`. A zero value means "section absent" — internal/docker's
// Client falls back to no --platform flag and no extra label.
type DockerConfig struct {
	DefaultPlatform string `toml:"default_platform"`
	LabelTemplate   string `toml:"label_template"` // "key=value" appended to every build
}

// IsZero reports whether the [docker] section was absent or empty.
func (dc DockerConfig) IsZero() bool {
	return dc.DefaultPlatform == "" && dc.LabelTemplate == ""
}

// CleanConfig is the [clean] section: the default root and --type filter
// `forgectl clean` uses when the corresponding flag is omitted. A zero value
// means "section absent" — internal/clean's Client falls back to its own
// built-in default (~/Projects, every Kind) for whichever fields are unset.
type CleanConfig struct {
	DefaultRoot string `toml:"default_root"`
	DefaultType string `toml:"default_type"`
}

// IsZero reports whether the [clean] section was absent or empty.
func (cc CleanConfig) IsZero() bool {
	return cc.DefaultRoot == "" && cc.DefaultType == ""
}

// SessionsConfig is the [sessions] section: how `forgectl sessions` reaches
// the cross-machine operational mart (an always-on Postgres holding the
// session index + runbook full-text index). A zero value means "section
// absent" — internal/sessions applies its own defaults (metrics/runbooks
// under ~/.claude, machine from the short hostname) and requires a DSN from
// FORGECTL_SESSIONS_DSN or --dsn. The DSN SHOULD omit the password: pgx
// resolves it from ~/.pgpass (libpq-compatible), keeping the secret outside
// the repo and the config file.
type SessionsConfig struct {
	DSN         string `toml:"dsn"`          // e.g. postgres://user@host:5433/sessions_mart
	Machine     string `toml:"machine"`      // provenance label; default: short hostname
	MetricsDir  string `toml:"metrics_dir"`  // default ~/.claude/metrics
	RunbooksDir string `toml:"runbooks_dir"` // default ~/.claude/cadence/runbooks
}

// IsZero reports whether the [sessions] section was absent or empty.
func (sc SessionsConfig) IsZero() bool {
	return sc.DSN == "" && sc.Machine == "" && sc.MetricsDir == "" && sc.RunbooksDir == ""
}

// ReviewConfig is the [review] section: which owners `forgectl review` fans
// its gh searches across. A zero value means "section absent" — the CLI layer
// applies its built-in default owner. Owner values are low-trust argv input;
// the search layer validates them against the anchored owner charset.
type ReviewConfig struct {
	Owners []string `toml:"owners"`
}

// IsZero reports whether the [review] section was absent or empty.
func (rc ReviewConfig) IsZero() bool {
	return len(rc.Owners) == 0
}

// DocsConfig is the [docs] section: extra root directories `forgectl docs`
// indexes alongside its built-in defaults (cwd, ./docs), and the bind address
// `serve` uses when --addr is omitted. A zero value means "section absent" —
// internal/docs applies its own built-in defaults for whichever fields are
// unset.
type DocsConfig struct {
	Roots []string `toml:"roots"`
	Addr  string   `toml:"addr"`
}

// IsZero reports whether the [docs] section was absent or empty.
func (dc DocsConfig) IsZero() bool {
	return len(dc.Roots) == 0 && dc.Addr == ""
}

// PreflightConfig is the [preflight] section: `forgectl preflight`'s
// deterministic alignment inputs. A zero value means "section absent" —
// internal/preflight's LocateCatalog falls back to installed_plugins.json
// and then a cache-dir glob, and DefaultSet contributes nothing beyond the
// catalog's own core tier.
type PreflightConfig struct {
	CatalogPath string   `toml:"catalog_path"` // override auto-locate; direct path to the generated catalog.md
	DefaultSet  []string `toml:"default_set"`  // extra "plugin@marketplace" entries always folded into the core-tier target, independent of catalog tier
}

// IsZero reports whether the [preflight] section was absent or empty.
func (pc PreflightConfig) IsZero() bool {
	return pc.CatalogPath == "" && len(pc.DefaultSet) == 0
}

// Baked defaults for hearth's frozen OTLP transport. These are the values a
// hearth-on-Colima collector listens on out of the box; a user overrides them
// in [bench] only when running a non-default endpoint.
const (
	DefaultOTLPEndpoint = "http://localhost:16317"
	DefaultOTLPProtocol = "grpc"
)

// BenchConfig is the [bench] section: how forgectl discovers and wires the
// local bench (hearth telemetry stack, chronicle transcript retention). Repo
// paths fall back to environment variables and degrade to empty when unset; the
// OTLP transport carries baked defaults so a zero config still points at a
// standard hearth. Mirrors WorkflowConfig — this package owns only the on-disk
// schema, not the probing logic (that lives in internal/bench).
type BenchConfig struct {
	HearthDir    string `toml:"hearth_dir"`
	ChronicleDir string `toml:"chronicle_dir"`
	OTLPEndpoint string `toml:"otlp_endpoint"` // default http://localhost:16317
	OTLPProtocol string `toml:"otlp_protocol"` // default grpc
	Telemetry    bool   `toml:"telemetry"`
}

// ResolvedHearthDir resolves the hearth checkout: the configured value, else
// $HEARTH_DIR, else empty (the signal to degrade to not-configured). A leading
// ~/ is expanded.
func (bc BenchConfig) ResolvedHearthDir() string {
	return resolveDir(bc.HearthDir, "HEARTH_DIR")
}

// ResolvedChronicleDir resolves the chronicle checkout: the configured value,
// else $CHRONICLE_DIR, else empty. A leading ~/ is expanded.
func (bc BenchConfig) ResolvedChronicleDir() string {
	return resolveDir(bc.ChronicleDir, "CHRONICLE_DIR")
}

// ResolvedOTLPEndpoint returns the configured OTLP endpoint or the baked
// default when unset.
func (bc BenchConfig) ResolvedOTLPEndpoint() string {
	if bc.OTLPEndpoint != "" {
		return bc.OTLPEndpoint
	}
	return DefaultOTLPEndpoint
}

// ResolvedOTLPProtocol returns the configured OTLP protocol or the baked
// default when unset.
func (bc BenchConfig) ResolvedOTLPProtocol() string {
	if bc.OTLPProtocol != "" {
		return bc.OTLPProtocol
	}
	return DefaultOTLPProtocol
}

// resolveDir picks the configured value, falls back to an environment variable,
// and expands a leading ~/. An empty result means "unconfigured" — callers
// degrade rather than error.
func resolveDir(configured, envVar string) string {
	dir := configured
	if dir == "" {
		dir = os.Getenv(envVar)
	}
	if dir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dir
	}
	return expandTilde(dir, home)
}

// expandTilde expands a leading ~ or ~/ to the home directory. Mirrors the
// launcher's helper (internal/launch) — kept local so config stays a
// lower-level package with no launch import.
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
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

// WorkflowStateDir returns the directory holding per-workflow run-state
// sidecars that back `workflow run --resume` and `workflow status`:
// <os.UserConfigDir()>/forgectl/workflows/.state (macOS: ~/Library/Application
// Support/forgectl/workflows/.state; Linux: ~/.config/forgectl/workflows/.state).
// It nests under WorkflowsDir so a user's workflow files and the run state that
// tracks them share one base, and the leading dot keeps it out of the way of
// the *.workflow.toml files the loader globs beside it.
func WorkflowStateDir() (string, error) {
	dir, err := WorkflowsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".state"), nil
}

// TrustStorePath returns the on-disk path for the workflow-blessing trust
// store: <os.UserConfigDir()>/forgectl/trust.toml (macOS: ~/Library/Application
// Support/forgectl/trust.toml; Linux: ~/.config/forgectl/trust.toml). It derives
// from the same configDir() base as ConfigPath/NetCachePath, so all of them
// never drift. The store lists enrolled machine keys and is itself a blessed
// file — its .blessing sidecar sits alongside it (internal/bless).
func TrustStorePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "trust.toml"), nil
}

// NetCachePath returns the on-disk path for the internal/net reachability
// cache: <os.UserConfigDir()>/forgectl/net-cache.json (macOS: ~/Library/
// Application Support/forgectl/net-cache.json; Linux: ~/.config/forgectl/
// net-cache.json). It derives from the same configDir() base as ConfigPath
// and WorkflowsDir, so all three never drift.
func NetCachePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "net-cache.json"), nil
}

// DockerLastTagPath returns the on-disk path for the `forgectl docker`
// last-built-tag cache: <os.UserConfigDir()>/forgectl/docker-lasttag (macOS:
// ~/Library/Application Support/forgectl/docker-lasttag; Linux:
// ~/.config/forgectl/docker-lasttag). It derives from the same configDir()
// base as ConfigPath/NetCachePath, so all three never drift. `run`/`shell`
// read this to reuse the tag `build` most recently produced when --tag is
// omitted.
func DockerLastTagPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "docker-lasttag"), nil
}

// PrReviewedPath returns the on-disk path for the `forgectl pr` reviewed-state
// store: <os.UserConfigDir()>/forgectl/pr-reviewed.json (macOS: ~/Library/
// Application Support/forgectl/pr-reviewed.json; Linux: ~/.config/forgectl/
// pr-reviewed.json). It derives from the same configDir() base as every other
// forgectl path, so they never drift. The store maps a PR's "owner/repo#N"
// breadcrumb form to the timestamp it was last marked reviewed (internal/pr).
func PrReviewedPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pr-reviewed.json"), nil
}

// ReviewReviewedPath returns the on-disk path for the `forgectl review`
// reviewed-state store: <os.UserConfigDir()>/forgectl/review-reviewed.json.
// Deliberately separate from PrReviewedPath's store — review keys are
// host-qualified ("github.com/owner/repo#N") and span issues, so sharing the
// pr file would mix two key dialects in one map. Same configDir() base as
// every other forgectl path, so they never drift.
func ReviewReviewedPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "review-reviewed.json"), nil
}

// PrSessionsDir returns the forgectl-owned directory that holds `forgectl pr`
// session breadcrumbs: <os.UserConfigDir()>/forgectl/pr-sessions (macOS:
// ~/Library/Application Support/forgectl/pr-sessions; Linux:
// ~/.config/forgectl/pr-sessions). It derives from the same configDir() base
// as ConfigPath/WorkflowsDir/NetCachePath, so all four never drift. The
// breadcrumb location check (internal/pr) enforces that a breadcrumb path
// resolves to inside this dir before any `git -C <workspace>` can run.
func PrSessionsDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pr-sessions"), nil
}

// PrFindingsDir returns the forgectl-owned directory that holds `forgectl pr`
// local-review findings: <os.UserConfigDir()>/forgectl/pr-findings (macOS:
// ~/Library/Application Support/forgectl/pr-findings; Linux:
// ~/.config/forgectl/pr-findings). It derives from the same configDir() base
// as ConfigPath/WorkflowsDir/PrSessionsDir, so all of them never drift.
// Findings are the deliverable of a local clean-room review and must outlive
// the disposable workspace, so they live here rather than under the OS temp
// root.
func PrFindingsDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pr-findings"), nil
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

// ErrNoLegacyLaunch signals that no legacy claunch.conf exists at the resolved
// path — the expected "nothing to import / nothing to shadow" outcome, distinct
// from a path-resolution or decode failure (which callers surface as real
// errors). Test for it with errors.Is.
var ErrNoLegacyLaunch = errors.New("no legacy claunch.conf found")

// LoadLegacyLaunch reads a legacy claunch.conf into a LaunchConfig — the same
// TOML shape as [launch] ([defaults] + [[project]]) — and returns the resolved
// legacy path alongside, so callers can report it without recomputing it via
// LegacyLaunchPath(). The error distinguishes the three outcomes callers care
// about: nil on success; ErrNoLegacyLaunch (wrapped with the path) when the file
// is simply absent; and a wrapped path-resolution or decode error otherwise.
// Callers decide leniency — resolveLaunchConfig ignores any error and falls
// through to config.toml; runClaunchImport surfaces absent vs unreadable
// distinctly.
func LoadLegacyLaunch() (LaunchConfig, string, error) {
	path, err := LegacyLaunchPath()
	if err != nil {
		return LaunchConfig{}, "", fmt.Errorf("resolve legacy claunch path: %w", err)
	}
	var lc LaunchConfig
	if _, err := toml.DecodeFile(path, &lc); err != nil {
		if os.IsNotExist(err) {
			return LaunchConfig{}, path, fmt.Errorf("%w at %s", ErrNoLegacyLaunch, path)
		}
		return LaunchConfig{}, path, fmt.Errorf("read legacy claunch.conf at %s: %w", path, err)
	}
	return lc, path, nil
}

// ValidateLegacyLaunch decodes the legacy claunch.conf and returns any parse
// error. A missing file is valid (nil). `forgectl launch doctor` uses this as a
// standalone health check to surface a broken legacy file instead of
// misreporting it as "no profiles configured".
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
