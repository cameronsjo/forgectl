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
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
//	effort = ""          # low|medium|high|xhigh|max; unset = derived from the model
//	[[launch.project]]
//	match = "~/Projects/minute"
//	model = "sonnet"
//
//	[net]                # forgectl net — cached reachability probe
//	probe_host = "1.1.1.1"           # baked default; public host
//	probe_port = 443
//	ttl_seconds = 60
//	timeout_ms = 1000
//	[proxy]              # forgectl proxy — named current-shell proxy profiles
//	[proxy.profiles.work]
//	http_proxy  = "http://proxy.example:8080"
//	https_proxy = "http://proxy.example:8080"
//	no_proxy    = "localhost,127.0.0.1"
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
//	[sessions]           # forgectl sessions — cross-machine operational concordance ETL
//	dsn     = "postgres://user@host:5433/concordance" # password via ~/.pgpass; env FORGECTL_SESSIONS_DSN wins
//	machine = ""                     # provenance label; default: short hostname
//	[github]             # deployment-wide GitHub host (spans projects + review)
//	host = ""                        # GitHub hostname, e.g. "github.example.com"; empty = github.com
//	[projects]           # forgectl projects — GitHub inventory scope
//	owners = ["your-login"]          # gh repo list scope; unset/[] = authenticated login
//	[[projects.wings]]   # optional: file these repos under <root>/<name>/<repo>, not the host tree
//	name  = "cadence-ecosystem"      # directory directly under the projects root
//	repos = ["cameronsjo/cadence"]   # "owner/name", matched case-insensitively
//	[review]             # forgectl review — cross-project work inventory
//	owners = ["your-login"]          # gh search --owner scope; unset/[] = authenticated login
//	[review.gitea]       # forgectl review — additional Gitea source (opt-in)
//	enabled = false                  # false by default; the tea CLI must be on PATH
//	host    = "git.example.com"      # required when enabled
//	login   = "your-login"           # optional; omitted → tea's own configured default login
//	owners  = ["your-login"]         # tea --owner scope, independent of [review] owners
//	[docs]               # forgectl docs — local markdown reader
//	roots = ["~/Projects/notes"]     # extra root dirs indexed alongside cwd/./docs
//	addr  = "127.0.0.1:4712"         # --addr default when the flag is omitted
//	[preflight]          # forgectl preflight — plugin/catalog alignment
//	catalog_path = ""    # override auto-locate; direct path to the generated catalog.md
//	default_set  = []    # extra "plugin@marketplace" entries always folded into the core-tier target
//	[update]             # forgectl update — weekly package-manager + OS maintenance
//	roster  = []          # step names to run when --only is omitted; empty = every roster step
//	log_dir = ""          # transcript log directory; empty = <config dir>/update-logs
//	[pr]                 # forgectl pr — bulk-launch cap and clean-room reviewer posture
//	max_concurrent = 4   # live "pr-*" tmux windows allowed at once; <= 0 = default (4)
//	model  = ""          # reviewer model; unset = the ambient launch profile's
//	effort = ""          # reviewer effort; unset = the ambient profile's, re-derived when model is set
type Config struct {
	NoIcons   bool            `toml:"no_icons"`
	LogLevel  string          `toml:"log_level"`
	LogFile   string          `toml:"log_file"`
	Launch    LaunchConfig    `toml:"launch"`
	Workflow  WorkflowConfig  `toml:"workflow"`
	Net       NetConfig       `toml:"net"`
	Proxy     ProxyConfig     `toml:"proxy"`
	Bench     BenchConfig     `toml:"bench"`
	Docker    DockerConfig    `toml:"docker"`
	Clean     CleanConfig     `toml:"clean"`
	Sessions  SessionsConfig  `toml:"sessions"`
	Projects  ProjectsConfig  `toml:"projects"`
	Review    ReviewConfig    `toml:"review"`
	Docs      DocsConfig      `toml:"docs"`
	Preflight PreflightConfig `toml:"preflight"`
	Update    UpdateConfig    `toml:"update"`
	Pr        PrConfig        `toml:"pr"`
	Github    GithubConfig    `toml:"github"`
	launchSet bool
	// decodeDegraded records that the config file existed but failed to
	// decode, so this Config may be missing sections the operator wrote.
	// Host-sensitive consumers (projects, review) must refuse loudly rather
	// than run against a silently-defaulted github.com — see DecodeDegraded.
	decodeDegraded bool
}

// DecodeDegraded reports whether the loaded config file failed to decode and
// this Config is a partial fallback. A tolerant Load() is right for launch
// profiles, but a GitHub-Enterprise deployment whose [github] host line was
// lost to a parse error would silently query public github.com and stamp rows
// as that host's data — so the projects and review seams check this flag and
// refuse with a config error instead.
func (c Config) DecodeDegraded() bool {
	return c.decodeDegraded
}

// HasLaunchSection distinguishes an explicitly present but empty [launch]
// table from a missing table. The distinction is authoritative during legacy
// migration: even an empty table shadows the compatibility source.
func (c Config) HasLaunchSection() bool {
	return c.launchSet || !c.Launch.IsZero()
}

// LaunchConfig is the [launch] section: base defaults plus directory-keyed
// project overrides for the `forgectl launch` command group. The resolution
// logic (longest-prefix match, merge) lives in internal/launch — this package
// owns only the on-disk schema.
type LaunchConfig struct {
	Defaults LaunchDefaults  `toml:"defaults"`
	Projects []LaunchProject `toml:"project"`

	// UsageStats is the informed opt-in for local launch statistics (#240).
	// Absent and explicit false are both disabled, and nothing but an operator
	// editing this key may set it: no migration, fallback, environment
	// variable, init, doctor, or stats path ever writes true here. See
	// internal/launch/usage.go for exactly what a recorded row contains.
	UsageStats bool `toml:"usage_stats"`
}

// LaunchDefaults is [launch.defaults]: the base posture applied when no project
// matches (and the floor a matching project overrides). AllowDanger is a pointer
// so an explicit `false` is distinguishable from "unset".
//
// Effort is a plain string, not a pointer, because there is no user for
// "explicitly emit no --effort flag": an unset value already derives a level
// from the model (launch.EffortForModel), and an unmapped model already emits
// nothing. The whole point of the key is raising or lowering a derived level,
// not suppressing one.
type LaunchDefaults struct {
	Harness         string            `toml:"harness"` // claude (default) | codex | pi
	Model           string            `toml:"model"`
	Provider        string            `toml:"provider"` // Pi provider; empty = Pi's own default
	Effort          string            `toml:"effort"`   // low|medium|high|xhigh|max; unset = derived from Model
	PermissionMode  string            `toml:"permission_mode"`
	AllowDanger     *bool             `toml:"allow_danger"`
	ApprovalPolicy  string            `toml:"approval_policy"` // Codex: untrusted|on-request|never
	Sandbox         string            `toml:"sandbox"`         // Codex: read-only|workspace-write|danger-full-access
	Env             map[string]string `toml:"env"`
	AddDir          []string          `toml:"add_dir"`
	BinaryPath      string            `toml:"binary_path"`       // explicit claude path; env FORGECTL_CLAUDE_BIN wins
	CodexBinaryPath string            `toml:"codex_binary_path"` // env FORGECTL_CODEX_BIN wins
	PiBinaryPath    string            `toml:"pi_binary_path"`    // env FORGECTL_PI_BIN wins
}

// LaunchProject is one [[launch.project]] directory-keyed override block.
type LaunchProject struct {
	Match          string            `toml:"match"`
	Harness        string            `toml:"harness"`
	Model          string            `toml:"model"`
	Provider       string            `toml:"provider"`
	Effort         string            `toml:"effort"`
	PermissionMode string            `toml:"permission_mode"`
	AllowDanger    *bool             `toml:"allow_danger"`
	ApprovalPolicy string            `toml:"approval_policy"`
	Sandbox        string            `toml:"sandbox"`
	Env            map[string]string `toml:"env"`
	AddDir         []string          `toml:"add_dir"`
}

// IsZero reports whether the [launch] section was absent or empty — the signal
// the launcher uses to fall back to a legacy claunch.conf.
//
// usage_stats counts toward non-empty, which changes how one specific operator
// is routed: someone holding both a claunch.conf and a config.toml containing
// nothing but the opt-in used to get wholesale legacy import and now gets the
// additive shadow-merge in MergeLegacyIntoLaunch. Every legacy setting survives
// either route — the difference is which code path carries it, and that is
// worth stating here because the opt-in reads like it should be inert.
func (lc LaunchConfig) IsZero() bool {
	return len(lc.Projects) == 0 && lc.Defaults.isZero() && !lc.UsageStats
}

// WorkflowConfig is the [workflow] section: extra strip-list entries the
// `strip` step adds when a workflow file's [[step]] omits `globs`. It WIDENS
// quarantine.DefaultTargets (#20) rather than replacing it — the built-in
// clean room is forgectl's own control, not something an operator narrows from
// this key by accident; see quarantine.stripFallback. To use a narrower list,
// set `globs` on the [[step]] itself.
type WorkflowConfig struct {
	StripGlobs []string `toml:"strip_globs"`
}

// IsZero reports whether the [workflow] section was absent or empty.
func (wc WorkflowConfig) IsZero() bool {
	return len(wc.StripGlobs) == 0
}

// NetConfig is the [net] section: the endpoint `forgectl net` probes for
// reachability, and how long a cached answer stays fresh. A zero value means
// "section absent" — internal/net's Client applies its own built-in defaults
// (probe_host 1.1.1.1, probe_port 443, ttl_seconds 60, timeout_ms 1000) for
// whichever fields are left unset. Those defaults point at a public host, so
// an unconfigured [net] answers "is the internet up" — set probe_host to an
// internal-only endpoint for an internal-network answer.
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

// ProxyConfig is the [proxy] section: named profiles whose values are emitted
// only by `forgectl proxy use NAME` for a shell wrapper to capture and eval.
// The generic config renderer deliberately exposes profile names but, because
// Profiles is a map, applies its map-value redaction policy to every value.
type ProxyConfig struct {
	Profiles map[string]ProxyProfile `toml:"profiles"`
}

// IsZero reports whether no named proxy profiles are configured.
func (pc ProxyConfig) IsZero() bool {
	return len(pc.Profiles) == 0
}

// ProxyProfile is one [proxy.profiles.NAME] table. Each configured value is
// copied to both its lower- and upper-case environment spelling; an omitted
// value unsets both spellings so a profile switch cannot retain stale halves.
// Values are sensitive and must never appear in logs, errors, or ordinary
// config/status output.
type ProxyProfile struct {
	HTTPProxy  string `toml:"http_proxy"`
	HTTPSProxy string `toml:"https_proxy"`
	AllProxy   string `toml:"all_proxy"`
	NoProxy    string `toml:"no_proxy"`
}

// IsZero reports whether the profile sets no proxy value at all.
func (p ProxyProfile) IsZero() bool {
	return p.HTTPProxy == "" && p.HTTPSProxy == "" && p.AllProxy == "" && p.NoProxy == ""
}

// The methods below make the never-print guarantee a property of the type
// rather than of every call site that happens to hold one. They mirror
// exec.SecretArg and backend.BootstrapCommand: every rendering interface the
// standard library reaches for — fmt, slog, encoding/json, encoding —
// answers with exec.Redacted. Reading a field explicitly still works, so the
// one sanctioned sink (internal/proxy's shell protocol) is unaffected.
//
// The guarantee has one documented exception, and it is a property of fmt,
// not of these methods: fmt consults Formatter/Stringer/GoStringer only when
// reflect.Value.CanInterface reports true, which is false for a value reached
// through an UNEXPORTED struct field. A holder keeping a Config,
// ProxyConfig, or ProxyProfile in an unexported field therefore renders the
// fields verbatim under %v/%+v — and slog's TextHandler renders a
// non-TextMarshaler value with exactly fmt.Sprintf("%+v", v). Closing that
// would mean holding the values in a closure the way exec.SecretArg does,
// which the TOML decoder's need for settable exported fields rules out. So:
// hold a proxy value in an exported field, or not at all.
// TestProxyProfile_RedactionIsScopedToExportedHolders pins this.

func (ProxyProfile) String() string   { return exec.Redacted }
func (ProxyProfile) GoString() string { return exec.Redacted }

func (ProxyProfile) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(exec.Redacted))
		return
	}
	_, _ = io.WriteString(f, exec.Redacted)
}

func (ProxyProfile) LogValue() slog.Value { return slog.StringValue(exec.Redacted) }

func (ProxyProfile) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(exec.Redacted)), nil
}

func (ProxyProfile) MarshalText() ([]byte, error) { return []byte(exec.Redacted), nil }

// ErrProxyProfileNotEncodable reports an attempt to TOML-encode a profile.
var ErrProxyProfileNotEncodable = errors.New(
	"config: a proxy profile cannot be TOML-encoded; write the [proxy.profiles] section from its fields")

// MarshalTOML makes a TOML encode of a profile fail loudly instead of
// destroying it. The BurntSushi encoder honors encoding.TextMarshaler, so
// MarshalText above — added for the redaction guarantee on the READ side —
// would otherwise rewrite a whole [proxy.profiles.NAME] table as the scalar
// `NAME = "[redacted]"`. There is no UnmarshalText, so that output does not
// even round-trip: the next load fails to decode the section. Silent data
// loss is the inverse of the failure this method set exists to prevent, and
// the encoder consults toml.Marshaler before encoding.TextMarshaler, so
// returning an error here is what makes the write path fail closed.
//
// No caller TOML-encodes a Config today; this exists so the one added later
// gets an error rather than a truncated config file.
func (ProxyProfile) MarshalTOML() ([]byte, error) { return nil, ErrProxyProfileNotEncodable }

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
// the cross-machine operational concordance (an always-on Postgres holding the
// session index + runbook full-text index). A zero value means "section
// absent" — internal/sessions applies its own defaults (metrics/runbooks
// under the Cadence state home, machine from the short hostname) and requires a DSN from
// FORGECTL_SESSIONS_DSN or --dsn. The DSN SHOULD omit the password: pgx
// resolves it from ~/.pgpass (libpq-compatible), keeping the secret outside
// the repo and the config file.
type SessionsConfig struct {
	DSN         string `toml:"dsn"`          // e.g. postgres://user@host:5433/concordance
	Machine     string `toml:"machine"`      // provenance label; default: short hostname
	MetricsDir  string `toml:"metrics_dir"`  // default Cadence XDG state metrics
	RunbooksDir string `toml:"runbooks_dir"` // default Cadence XDG state runbooks
}

// IsZero reports whether the [sessions] section was absent or empty.
func (sc SessionsConfig) IsZero() bool {
	return sc.DSN == "" && sc.Machine == "" && sc.MetricsDir == "" && sc.RunbooksDir == ""
}

// ProjectsConfig is the [projects] section: which GitHub.com accounts
// `forgectl projects` enumerates. A zero value — the section absent, or
// `owners = []` written explicitly — means "the authenticated GitHub.com
// login", resolved once per operation by internal/githubauth.
//
// It is deliberately independent of ReviewConfig.Owners: which repos you jump
// between and which repos you triage work in are different policies, and
// neither list ever inherits from the other. Owner values are low-trust argv
// input; internal/githubauth validates them against the anchored owner
// charset and bounds the set before any subprocess is spawned.
type ProjectsConfig struct {
	Owners []string     `toml:"owners"`
	Wings  []WingConfig `toml:"wings"`
}

// IsZero reports whether the [projects] section was absent or empty. A section
// carrying only wings is present: `[[projects.wings]]` with no `owners` is a
// legitimate configuration (wings steer placement, owners steer enumeration),
// and reporting it absent would make the CLI seam skip a table the operator
// wrote.
func (pc ProjectsConfig) IsZero() bool {
	return len(pc.Owners) == 0 && len(pc.Wings) == 0
}

// WingConfig is one `[[projects.wings]]` entry: a named directory directly
// under the projects root that a listed set of repos is filed into, instead of
// the host/owner/name tree.
//
// This is PLACEMENT, and it is deliberately config rather than inference:
// where a NEW clone belongs is a judgment call about how the operator groups
// work, and disk state cannot answer it — a wing tells you what already lives
// there, not what should. Wing DISCOVERY is the mirror-image decision and is
// structural (a depth-1 directory holding at least one git repo), so a wing
// missing from this table is still listed; it just is not a clone target.
//
// Name lands verbatim as a filesystem path segment directly under the projects
// root, where it shares a namespace with the host trees — so it is validated
// against the same anchored charset a host segment is, and a name colliding
// with the configured GitHub host is refused at the CLI seam.
type WingConfig struct {
	Name  string   `toml:"name"`
	Repos []string `toml:"repos"` // "owner/name", matched case-insensitively
}

// ReviewConfig is the [review] section: which owners `forgectl review` fans
// its gh searches across, plus the opt-in Gitea source. A zero value means
// "section absent" — the GitHub source then resolves the authenticated
// GitHub.com login, exactly as an absent [projects] section does, and
// independently of it. Owner values are low-trust argv input; the search
// layer validates them against the anchored owner charset.
type ReviewConfig struct {
	Owners []string    `toml:"owners"`
	Gitea  GiteaConfig `toml:"gitea"`
}

// IsZero reports whether the [review] section was absent or empty.
func (rc ReviewConfig) IsZero() bool {
	return len(rc.Owners) == 0 && rc.Gitea.IsZero()
}

// PrConfig is the [pr] section: `forgectl pr pick`'s bulk-launch concurrency
// cap, plus the clean-room reviewer's model and effort. A zero value means
// "section absent" — internal/pr owns the built-in concurrency default
// (DefaultMaxConcurrentReviews) applied when MaxConcurrent is unset or
// non-positive, and an unset Model/Effort leaves the reviewer on the ambient
// launch profile's posture, per the house zero-means-absent convention every
// other *Config section in this file follows.
//
// SECURITY BOUNDARY — this section carries model and effort ONLY, and
// TestPrConfig_FieldSetIsPinned enforces that. The clean-room review's
// hardening (AllowDanger=false, PermissionMode="plan", StrictMCP=true,
// AddDir=nil) is forgectl's own control over a third party's checkout, not an
// operator preference. A `permission_mode` key here would let a config file
// turn that control off; adding one is a deliberate act that must break the
// pinning test first.
type PrConfig struct {
	MaxConcurrent int `toml:"max_concurrent"`
	// Model overrides the reviewer's model independently of whatever repo it
	// happens to be reviewing. Setting it re-derives Effort from the new model
	// (internal/pr launchInline) rather than carrying the ambient repo's level
	// across — see that call site for why filling only when empty is wrong.
	Model string `toml:"model"`
	// Effort overrides the reviewer's --effort outright, beating both the
	// ambient profile and the Model-derived level.
	Effort string `toml:"effort"`
}

// IsZero reports whether the [pr] section was absent or empty.
func (pc PrConfig) IsZero() bool {
	return pc.MaxConcurrent == 0 && pc.Model == "" && pc.Effort == ""
}

// GithubConfig is the [github] section: the one GitHub host this deployment's
// projects and review inventories talk to. A zero value means github.com. The
// section is deliberately deployment-wide rather than per-module: divergent
// per-module hosts would stamp clones and review keys with different hosts —
// exactly the mislabeling internal/githubauth's pin exists to prevent. The
// section name leaves room for a later [[github.hosts]] multi-source form
// (declined for now — forgectl#412).
//
// SECURITY BOUNDARY — this section carries the host ONLY, and
// TestGithubConfig_FieldSetIsPinned enforces that. The value is low-trust
// config input: internal/githubauth.ResolveHost validates it against an
// anchored hostname allowlist before it may reach a subprocess env, and any
// non-default host has the gh token env vars scrubbed so a hostile host line
// cannot redirect an ambient credential. Adding a field here that widens what
// a config file can steer is a deliberate act that must break the pinning
// test first.
type GithubConfig struct {
	Host string `toml:"host"`
}

// IsZero reports whether the [github] section was absent or empty.
func (gc GithubConfig) IsZero() bool {
	return gc.Host == ""
}

// GiteaConfig is the [review.gitea] section: forgectl review's opt-in second
// source, a self-hosted Gitea instance enumerated over the tea CLI (Phase
// C). A zero value means "section absent" or disabled — the review module
// omits the source entirely rather than constructing one doomed to error at
// Items() time. Host and Owners are low-trust config input headed for an
// argv and a persisted store key: Host is validated at review.NewGitea
// construction (an anchored hostname charset, since it lands in every
// Item's Host field); Owners ride the same anchored owner/repo charset
// every review/pr owner does, validated per-query in Items.
type GiteaConfig struct {
	Enabled bool     `toml:"enabled"`
	Host    string   `toml:"host"`
	Login   string   `toml:"login"`
	Owners  []string `toml:"owners"`
}

// IsZero reports whether the [review.gitea] section was absent or empty.
func (gc GiteaConfig) IsZero() bool {
	return !gc.Enabled && gc.Host == "" && gc.Login == "" && len(gc.Owners) == 0
}

// DocsConfig is the [docs] section: extra root directories `forgectl docs`
// indexes alongside its built-in defaults (cwd, ./docs), and the bind address
// `serve` uses when --addr is omitted. A zero value means "section absent" —
// internal/docs applies its own built-in defaults for whichever fields are
// unset.
type DocsConfig struct {
	Roots []string `toml:"roots"`
	Addr  string   `toml:"addr"`
	// RootKinds overrides docs.detectRootKind's filesystem-based inference
	// for a root, keyed by the root path exactly as it appears in Roots (or
	// in the positional arguments to `docs serve`/`docs list`) — not a
	// canonicalized form. Values are "docs" or "vault"; any other value is a
	// config error (internal/cli's docsIndexOptions rejects it, naming the
	// key, the value, and the two allowed values).
	RootKinds map[string]string `toml:"root_kinds"`
}

// IsZero reports whether the [docs] section was absent or empty.
func (dc DocsConfig) IsZero() bool {
	return len(dc.Roots) == 0 && dc.Addr == "" && len(dc.RootKinds) == 0
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

// UpdateConfig is the [update] section: `forgectl update`'s persisted
// defaults. A zero value means "section absent" — internal/update's Client
// runs its full built-in roster (DefaultSteps) and internal/cli falls back
// to its own default log directory.
type UpdateConfig struct {
	// Roster names the steps to run when --only is omitted (matched against
	// the roster's Step.Name values). Empty means every roster step — the
	// same "unrestricted" meaning --only's absence has on the CLI.
	Roster []string `toml:"roster"`
	// LogDir overrides the default directory for the timestamped
	// transcript log. Empty falls back to UpdateLogDir().
	LogDir string `toml:"log_dir"`
}

// IsZero reports whether the [update] section was absent or empty.
func (uc UpdateConfig) IsZero() bool {
	return len(uc.Roster) == 0 && uc.LogDir == ""
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
	return d.Harness == "" && d.Model == "" && d.Provider == "" && d.Effort == "" && d.PermissionMode == "" &&
		d.AllowDanger == nil && d.ApprovalPolicy == "" && d.Sandbox == "" &&
		len(d.Env) == 0 && len(d.AddDir) == 0 && d.BinaryPath == "" &&
		d.CodexBinaryPath == "" && d.PiBinaryPath == ""
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
		// The config file could not even be LOCATED — any [github] host the
		// operator wrote is unreadable, so the host-sensitive seams must
		// refuse rather than silently query github.com with tokens
		// unscrubbed. This is the sibling of the malformed-file case below,
		// reached one step earlier.
		return Config{decodeDegraded: true}
	}
	return LoadPath(path)
}

// LoadPath reads the already-resolved config path for a process attempt. It
// keeps Load's tolerant startup behavior while preventing migration callers
// from recomputing HOME/XDG after their authoritative boundary was captured.
func LoadPath(path string) Config {
	data, err := ReadPath(path)
	if os.IsNotExist(err) {
		return Config{}
	}
	cfg, decodeErr := DecodeStrict(data)
	if err == nil {
		err = decodeErr
	}
	if err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to decode config file; using built-in defaults for unreadable sections.",
			"path", termsafe.QuotePath(path), "error", termsafe.SafeLine(err.Error()))
		cfg.decodeDegraded = true
	}
	return cfg
}

// DecodeStrict decodes an immutable config snapshot and retains table
// presence metadata. Migration uses it only after acquiring the writer lock.
func DecodeStrict(data []byte) (Config, error) {
	var cfg Config
	if len(data) == 0 {
		return cfg, nil
	}
	meta, err := toml.Decode(string(data), &cfg)
	cfg.launchSet = meta.IsDefined("launch")
	return cfg, err
}

// Validate decodes the config file and returns any parse error. A missing file
// is valid (built-in defaults). Used by `forgectl launch doctor` to surface a
// malformed config that Load() tolerated with a warning.
func Validate() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return ValidatePath(path)
}

// ValidatePath strictly decodes the already-resolved config path. A missing
// file remains valid and selects built-in defaults.
func ValidatePath(path string) error {
	data, err := ReadPath(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = DecodeStrict(data)
	return err
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

	f, err := OpenAppendFile(path)
	if err != nil {
		return os.Stderr, nopCloser{}
	}

	// Prune old daily log files (auto mode only; best-effort, non-fatal).
	if logFile == "" {
		pruneOldLogs(filepath.Dir(path))
	}

	return f, f
}

// OpenAppendFile ensures path's directory exists (MkdirAll 0700) and opens
// path itself for append (O_CREATE|O_APPEND|O_WRONLY, 0600) — the shared
// mkdir+open trio behind every forgectl log-file writer. Exported so a
// module's own log-file plumbing (e.g. `forgectl update`'s per-run
// transcript log) can reuse it instead of re-deriving the same two calls;
// unlike openLogWriter, it returns the error rather than silently degrading,
// since callers differ on what "silently degrading" should look like (a
// bare fallback to stderr here, a warned fallback there).
func OpenAppendFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	return f, nil
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
	pruneLogsMatching(dir, "forgectl-", ".log", "2006-01-02")
}

// PruneUpdateLogs deletes update-YYYYMMDD-HHMMSS.log files in dir that are
// older than logKeepDays days — `forgectl update`'s equivalent of
// pruneOldLogs above, since its transcript logs use their own timestamped
// naming (one file per run, not one per day) rather than the daily-rotated
// scheme. Exported (unlike pruneOldLogs) because the update-logs directory
// is opened from internal/cli, not this package; errors are silently
// ignored — log pruning is never fatal. Safe to call regardless of whether
// dir is the default (UpdateLogDir()) or a user override ([update] log_dir):
// it only ever touches files matching this exact pattern, never anything
// else that might share the directory.
func PruneUpdateLogs(dir string) {
	pruneLogsMatching(dir, "update-", ".log", "20060102-150405")
}

// pruneLogsMatching deletes files in dir named prefix+<timestamp>+suffix
// whose embedded timestamp (parsed per layout) is older than logKeepDays —
// the shared body behind pruneOldLogs (daily forgectl-*.log) and
// PruneUpdateLogs (per-run update-*.log), which differ only in naming
// scheme. Errors are silently ignored — log pruning is never fatal.
func pruneLogsMatching(dir, prefix, suffix, layout string) {
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
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
		t, err := time.Parse(layout, dateStr)
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

// UpdateLogDir returns the default directory for `forgectl update`'s
// timestamped transcript logs: <os.UserConfigDir()>/forgectl/update-logs
// (macOS: ~/Library/Application Support/forgectl/update-logs; Linux:
// ~/.config/forgectl/update-logs). It derives from the same configDir()
// base as every other forgectl path, so it never drifts from them. [update]
// log_dir overrides this; callers should prefer that when set.
func UpdateLogDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update-logs"), nil
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

// DocsServerPath returns the LEGACY single-file discovery record:
// <os.UserConfigDir()>/forgectl/docs-server.json (macOS: ~/Library/Application
// Support/forgectl/docs-server.json; Linux: ~/.config/forgectl/
// docs-server.json). It derives from the same configDir() base as every other
// forgectl path, so none of them drift.
//
// This path is now READ-ONLY. Current servers neither write nor remove it; they
// publish one immutable record per generation under DocsServersDir instead,
// because a single shared pathname cannot be owned — two overlapping servers
// both wrote this file, and whichever stopped first deleted the other's
// discoverability (forgectl#277).
//
// It remains here so a new client can still find a server started by an older
// binary, and so rolling back to an older binary leaves that binary's record
// intact. Nothing in forgectl deletes or rewrites it.
//
// The record exists at all because `docs open` STEERS an already-running reader
// rather than launching one, and the bound address is not knowable in advance —
// the default bind uses port 0, so the OS assigns it.
func DocsServerPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "docs-server.json"), nil
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

// ResumeStoreDir returns the forgectl-owned directory that holds `forgectl
// resume` session snapshots: <os.UserConfigDir()>/forgectl/resume-sessions
// (macOS: ~/Library/Application Support/forgectl/resume-sessions; Linux:
// ~/.config/forgectl/resume-sessions). It derives from the same configDir()
// base as ConfigPath/PrSessionsDir/NetCachePath, so none of them drift.
//
// One JSON file per Claude Code session id. The store exists because two
// things a session owns do NOT survive its exit: the /rename name, and the
// task bodies under ~/.claude/tasks — Claude Code deletes those. It also
// carries the session-id → task-directory association, which nothing on disk
// records durably (internal/resume).
func ResumeStoreDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "resume-sessions"), nil
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
var (
	ErrNoLegacyLaunch   = errors.New("no legacy claunch.conf found")
	ErrConfigNonRegular = errors.New("config path does not resolve to a regular file")
)

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
	return stripLegacyUsageOptIn(lc), path, nil
}

// MergeLegacyIntoLaunch performs the additive-only merge behind the
// shadow-scenario auto-migration (#114): legacy claunch.conf fields are
// folded into cfg.Launch only where the corresponding config.toml field is
// still zero-valued, and legacy [[project]] entries are appended only for
// match paths not already present in [[launch.project]]. It never overwrites
// or duplicates anything already set in [launch] — added counts how many
// defaults fields and project entries actually came from legacy, so a caller
// can tell "nothing new" (0, the legacy file is fully superseded) from a real
// merge.
func MergeLegacyIntoLaunch(cfg Config, legacy LaunchConfig) (merged LaunchConfig, added int) {
	merged = cfg.Launch
	d := &merged.Defaults
	l := legacy.Defaults

	fillString := func(dst *string, src string) {
		if *dst == "" && src != "" {
			*dst = src
			added++
		}
	}
	fillString(&d.Harness, l.Harness)
	fillString(&d.Model, l.Model)
	fillString(&d.Provider, l.Provider)
	fillString(&d.Effort, l.Effort)
	fillString(&d.PermissionMode, l.PermissionMode)
	fillString(&d.ApprovalPolicy, l.ApprovalPolicy)
	fillString(&d.Sandbox, l.Sandbox)
	fillString(&d.BinaryPath, l.BinaryPath)
	fillString(&d.CodexBinaryPath, l.CodexBinaryPath)
	fillString(&d.PiBinaryPath, l.PiBinaryPath)

	if d.AllowDanger == nil && l.AllowDanger != nil {
		v := *l.AllowDanger
		d.AllowDanger = &v
		added++
	}
	if len(d.Env) == 0 && len(l.Env) > 0 {
		d.Env = l.Env
		added++
	}
	if len(d.AddDir) == 0 && len(l.AddDir) > 0 {
		d.AddDir = l.AddDir
		added++
	}

	existing := make(map[string]bool, len(merged.Projects))
	for _, p := range merged.Projects {
		existing[p.Match] = true
	}
	for _, p := range legacy.Projects {
		if existing[p.Match] {
			continue
		}
		merged.Projects = append(merged.Projects, p)
		existing[p.Match] = true
		added++
	}

	return merged, added
}

// nopCloser is an io.Closer that does nothing — stdlib io.NopCloser wraps
// io.Reader, not a generic Closer, so we keep this small private type.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }
