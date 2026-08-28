package cli

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// This file scaffolds config.toml end to end (`forgectl init`), one template
// block per section, each appended only if that section is absent —
// preserving every hand-edit and comment already on disk. It deliberately
// never round-trips through toml.NewEncoder (that would decode-then-re-encode
// and strip every comment); see hasSection below, and newInitCmd's append
// loop that consults it, for the same append-if-absent discipline
// `forgectl launch init` already applies to just [launch]. Template values are verified against their owning package's
// actual fallback (Resolved*/IsZero methods, or the Client's own zero-value
// behavior) rather than guessed — several match config.Config's top-of-file
// doc comment, but that comment itself drifted from the real code in a few
// spots (see the per-const comments below).

// hostScalarsScaffold is the config.toml preamble: the three keys Config
// carries at document root with no table header of their own. It has no
// leading blank line (it is meant to sit at byte 0 of the file) and is
// prepended, never appended — see prependHostScalars.
const hostScalarsScaffold = `# ── forgectl: global settings (bare keys — must precede every [section]) ────
no_icons  = false # true disables emoji/glyph icons in output
log_level = "off" # off | debug | info | warn | error
log_file  = ""    # empty = auto (daily rotation, 7 days kept); "-" = stderr

`

// workflowScaffold is the [workflow] section: extra strip-list globs added to
// the built-in set (WorkflowConfig.IsZero — config.go). An empty list is
// already the no-op default; quarantine.DefaultTargets applies either way.
const workflowScaffold = `
# ── workflow: extra strip-list globs (forgectl workflow) ───────────────────
[workflow]
strip_globs = [] # ADDED to quarantine.DefaultTargets; to narrow, set globs on the [[step]]
`

// netScaffold is the [net] section. Values mirror internal/net's own baked
// constants (defaultProbeHost/-Port/-TTLSeconds/-TimeoutMs), so an untouched
// scaffold is a no-op posture.
const netScaffold = `
# ── net: cached reachability probe (forgectl net) ───────────────────────────
[net]
probe_host  = "1.1.1.1" # baked default; public — set an internal-only host for an internal-network answer
probe_port  = 443       # baked default
ttl_seconds = 60        # cached result freshness window, seconds
timeout_ms  = 1000      # probe dial timeout, milliseconds
`

// proxyScaffold names the schema without installing a machine-specific
// endpoint. Profile values may contain credentials, so the example remains
// commented and `forgectl config` exposes only configured profile names.
const proxyScaffold = `
# ── proxy: named current-shell proxy profiles (forgectl proxy) ──────────────
[proxy]
# [proxy.profiles.work]
# http_proxy  = "http://proxy.example:8080"
# https_proxy = "http://proxy.example:8080"
# all_proxy   = "socks5://proxy.example:1080"
# no_proxy    = "localhost,127.0.0.1"
`

// benchScaffold is the [bench] section. hearth_dir/chronicle_dir have no
// baked literal default — ResolvedHearthDir/ResolvedChronicleDir fall back to
// $HEARTH_DIR/$CHRONICLE_DIR and then empty (config.go), so they stay
// commented rather than guessing a checkout path. otlp_endpoint/-protocol do
// have baked constants (config.DefaultOTLPEndpoint/-Protocol) and are written
// active.
const benchScaffold = `
# ── bench: local bench interop — hearth telemetry, chronicle transcripts ───
[bench]
# hearth_dir    = ""  # else $HEARTH_DIR
# chronicle_dir = ""  # else $CHRONICLE_DIR
otlp_endpoint = "http://localhost:16317" # baked default (config.DefaultOTLPEndpoint)
otlp_protocol = "grpc"                   # baked default (config.DefaultOTLPProtocol)
telemetry     = false                    # opt-in: inject OTLP env into launches
`

// dockerScaffold is the [docker] section. Neither field has a baked default —
// WithDockerConfig only overrides Client's zero value when the config field is
// non-empty, so an absent [docker] means no --platform flag and no extra
// label (config.go's own doc comment says this explicitly). Both stay
// commented as examples rather than active defaults.
const dockerScaffold = `
# ── docker: build/run/shell defaults (forgectl docker) ──────────────────────
[docker]
# default_platform = "linux/amd64" # --platform default when set; unset = no --platform flag passed
# label_template    = ""           # extra "key=value" OCI label appended to every build
`

// cleanScaffold is the [clean] section. default_root does have a baked
// default (New's home+"/Projects", internal/clean/clean.go's
// defaultRootSubdir) so it is written active; default_type's baked default is
// already the empty string (every Kind).
const cleanScaffold = `
# ── clean: dep/build-dir reclaim defaults (forgectl clean) ──────────────────
[clean]
default_root = "~/Projects" # baked default when unset
default_type = ""           # empty = every kind; node|python|go|build
`

// sessionsScaffold is the [sessions] section. dsn has no baked default (it's
// required, via config or $FORGECTL_SESSIONS_DSN) and machine's baked default
// is the current machine's short hostname — neither is a value this template
// can bake in without lying on a different machine, so both stay commented.
// metrics_dir/runbooks_dir have state-home-derived baked defaults; sync.go
// (internal/sessions/sync.go) expands a leading ~ the same way config,
// launch and clean do, and an explicitly-configured metrics_dir that doesn't exist on disk
// (a typo, or a path moved since) now fails loudly instead of silently
// syncing zero sessions. They stay commented so the absent-key fallback
// (the home-expanded default) applies.
const sessionsScaffold = `
# ── sessions: cross-machine operational concordance ETL (forgectl sessions) ───────
[sessions]
# dsn = "postgres://user@host:5433/concordance" # or $FORGECTL_SESSIONS_DSN; required
# machine = "" # default: short hostname
# metrics_dir  = ""  # default: ${XDG_STATE_HOME:-~/.local/state}/cadence/metrics; ~ expanded
# runbooks_dir = ""  # default: ${XDG_STATE_HOME:-~/.local/state}/cadence/runbooks; ~ expanded
`

// githubScaffold is the [github] section: the one GitHub host this
// deployment's projects and review inventories talk to. Commented rather than
// active: the baked default is github.com, and writing an active host here
// would pin a GitHub Enterprise hostname into every machine's config.toml.
// Setting it requires a stored credential for that host (`gh auth login
// --hostname <host>`) — on a non-default host forgectl scrubs the gh token
// env vars, so the hosts.yml credential is the only one gh can use.
const githubScaffold = `
# ── github: deployment-wide GitHub host (projects + review) ─────────────────
[github]
# host = "" # GitHub hostname, e.g. "github.example.com"; empty = github.com. Needs: gh auth login --hostname <host>
`

// projectsScaffold is the [projects] section. Commented rather than active:
// there is no baked owner to mirror any more — an unset list resolves the
// authenticated GitHub.com login at run time (internal/githubauth) — so
// writing an active value here would pin one machine's account into every
// host's config.toml, the same reasoning preflightScaffold and prScaffold
// give for staying commented.
const projectsScaffold = `
# ── projects: cross-forge project inventory (forgectl projects) ─────────────
[projects]
# owners = ["your-login"] # gh repo list scope; unset or [] = authenticated GitHub.com login
`

// reviewScaffold is the [review] section. Commented for the same reason as
// projectsScaffold: an unset list resolves the authenticated GitHub.com login,
// and the two lists are independent — neither inherits from the other. An
// existing active `owners` stays as written; init never rewrites a present
// section.
const reviewScaffold = `
# ── review: cross-project work inventory (forgectl review) ─────────────────
[review]
# owners = ["your-login"] # gh search --owner scope; unset or [] = authenticated GitHub.com login
`

// docsScaffold is the [docs] section. roots' baked default is cwd + ./docs
// (internal/cli/docs_roots.go), not a fixed extra path — "~/Projects/notes"
// is an example of an extra root, not a default, so it stays commented.
// addr's baked default is httpsrv.LoopbackAddr ("127.0.0.1:0", a random
// port), not a fixed "127.0.0.1:4712" — config.Config's own top-of-file doc
// comment names that port, but it appears nowhere else in the codebase; this
// scaffold corrects the drift rather than propagating it.
const docsScaffold = `
# ── docs: local markdown reader (forgectl docs) ─────────────────────────────
[docs]
# roots = ["~/Projects/notes"] # extra root dirs indexed alongside cwd/./docs (example)
addr = "" # empty = 127.0.0.1 with a random port; set host:port to pin one
`

// preflightScaffold is the [preflight] section. Neither field has a baked
// literal default — PreflightConfig's zero value means LocateCatalog
// auto-locates the catalog (installed_plugins.json, then a cache-dir glob) and
// DefaultSet contributes nothing beyond the catalog's own core tier — so both
// stay commented rather than baking in a path that would be wrong on the next
// machine.
const preflightScaffold = `
# ── preflight: plugin/catalog alignment (forgectl preflight) ────────────────
[preflight]
# catalog_path = "" # override auto-locate; direct path to the generated catalog.md
# default_set  = [] # extra "plugin@marketplace" entries always folded into the core-tier target
`

// updateScaffold is the [update] section. Empty is the real default for both
// fields AND it means something in each case — an empty roster runs every
// roster step (UpdateConfig's own doc comment), and an empty log_dir falls back
// to config.UpdateLogDir() — so both are written active, mirroring
// workflowScaffold's `strip_globs = []` and docsScaffold's `addr = ""`.
const updateScaffold = `
# ── update: weekly package-manager + OS maintenance (forgectl update) ───────
[update]
roster  = [] # step names to run when --only is omitted; empty = every roster step
log_dir = "" # transcript log directory; empty = <config dir>/update-logs
`

// prScaffold is the [pr] section. Commented rather than active: the field's
// zero value is not a cap of zero, it is "unset", and internal/pr.Admit
// resolves any non-positive value to DefaultMaxConcurrentReviews. Writing
// `max_concurrent = 4` would bake today's built-in default into every host's
// config.toml and silently pin it there when that default next changes —
// the same reasoning preflightScaffold gives for staying commented.
const prScaffold = `
# ── pr: bulk review-window admission (forgectl pr) ──────────────────────────
[pr]
# max_concurrent = 4 # cap on concurrent review windows; unset or <1 uses the built-in default
`

// initSection is one scaffoldable block: a config.toml section (or, for the
// empty name, the host-scalar preamble) plus its annotated template.
type initSection struct {
	name     string // toml section key; "" is the host-scalar pseudo-section
	label    string // human label for the per-section report line
	template string
}

// initSections lists every scaffoldable block in file order. The host-scalar
// preamble MUST stay first — it is the only block prependHostScalars ever
// inserts ahead of existing content; every other block is appended in order
// by newInitCmd's loop, gated on hasSection. [launch] reuses launchScaffold
// (launch_init.go) directly rather than a second copy.
//
// This list is hand-maintained and therefore drifts: it silently omitted
// [preflight] and [update] for as long as those sections existed, so `forgectl
// init` — whose whole job is "scaffold EVERY section" — scaffolded 9 of 11.
// TestInitSections_CoversEveryStructSection derives the expected set from
// config.Config by reflection and fails the moment a new section lands here
// unscaffolded.
var initSections = []initSection{
	{"", "host scalars", hostScalarsScaffold},
	{"launch", "launch", launchScaffold},
	{"workflow", "workflow", workflowScaffold},
	{"net", "net", netScaffold},
	{"proxy", "proxy", proxyScaffold},
	{"bench", "bench", benchScaffold},
	{"docker", "docker", dockerScaffold},
	{"clean", "clean", cleanScaffold},
	{"sessions", "sessions", sessionsScaffold},
	{"github", "github", githubScaffold},
	{"projects", "projects", projectsScaffold},
	{"review", "review", reviewScaffold},
	{"docs", "docs", docsScaffold},
	{"preflight", "preflight", preflightScaffold},
	{"update", "update", updateScaffold},
	{"pr", "pr", prScaffold},
}

// initModule declares the full-scaffold convenience extension (ADR-0005). It
// claims no config section of its own — like configModule, it touches every
// section instead of owning one — so `forgectl init` writes every section's
// template in one pass, skipping whatever config.toml already defines.
var initModule = module.Manifest{
	Name: "init",
	Tier: module.TierExtension,
	New:  newInitCmd,
}

// newInitCmd builds `forgectl init`: for each block in initSections, append
// (or, for the host-scalar preamble, prepend) its template iff that block is
// not already present in config.toml. It never overwrites or reflows a
// section that's already there.
func newInitCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Scaffold every config.toml section with commented, sensibly-defaulted templates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			slog.Debug("Preparing to scaffold config.toml.")
			if err := refuseConfigMutationForLegacyBoundary(deps.LegacyBoundary); err != nil {
				return termsafe.Error(err)
			}
			path := ""
			if deps.LegacyBoundary != nil {
				path = deps.LegacyBoundary.ConfigPath
			} else {
				var err error
				path, err = config.ConfigPath()
				if err != nil {
					return termsafe.Error(err)
				}
			}
			out := cmd.OutOrStdout()
			added := 0
			var lines []string
			action, err := updateConfigLocked(path, nativeConfigWriterOps(), func(raw []byte) ([]byte, error) {
				data := raw
				for _, s := range initSections {
					if hasSection(data, s.name) {
						lines = append(lines, "already present: "+s.label)
						continue
					}
					if s.name == "" {
						data = append([]byte(s.template), data...)
					} else {
						data = append(data, []byte(s.template)...)
					}
					lines = append(lines, "added:            "+s.label)
					added++
				}
				return data, nil
			})
			if err != nil && !visibleWithoutDirectoryDurability(action, err) {
				return termsafe.Error(err)
			}
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "forgectl: config is visible, but directory durability and cross-process locking are unavailable on this platform")
			}
			for _, line := range lines {
				fmt.Fprintln(out, line)
			}

			slog.Info("Successfully scaffolded config.toml.", "path", termsafe.QuotePath(path), "sections_added", added)
			fmt.Fprintf(out, "\n%d section(s) added to %s\n", added, termsafe.QuotePath(path))
			return nil
		},
	}
}

// hasSection reports whether data already defines the named top-level TOML
// table — [name], [name.…], or [[name.…]] — matching real headers rather
// than a loose substring (generalizes hasLaunchSection's discipline in
// launch_init.go to any section name). name == "" is the host-scalar
// pseudo-section, which owns no table header of its own: presence there means
// any of its three bare keys (no_icons, log_level, log_file) appears anywhere
// in the file.
func hasSection(data []byte, name string) bool {
	if name == "" {
		return hasHostScalars(data)
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "["+name+"]" || strings.HasPrefix(t, "["+name+".") || strings.HasPrefix(t, "[["+name+".") {
			return true
		}
	}
	return false
}

// hostScalarKeyRe matches a bare host-scalar key assignment at the start of a
// (trimmed) line.
var hostScalarKeyRe = regexp.MustCompile(`^(no_icons|log_level|log_file)\s*=`)

// hasHostScalars reports whether data already assigns any of the three
// host-scalar keys.
func hasHostScalars(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if hostScalarKeyRe.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}
