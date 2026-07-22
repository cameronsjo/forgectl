package cli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
)

// This file scaffolds config.toml end to end (`forgectl init`), one template
// block per section, each appended only if that section is absent —
// preserving every hand-edit and comment already on disk. It deliberately
// never round-trips through toml.NewEncoder (that would decode-then-re-encode
// and strip every comment); see hasSection/appendLaunchSection below for the
// same append-if-absent discipline `forgectl launch init` already applies to
// just [launch]. Template values are verified against their owning package's
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

// workflowScaffold is the [workflow] section: the default strip-list glob
// fallback (WorkflowConfig.IsZero — config.go). An empty list is already the
// no-op default; quarantine.DefaultTargets is the built-in fallback the
// `strip` step applies when this stays empty.
const workflowScaffold = `
# ── workflow: default strip-list glob fallback (forgectl workflow) ─────────
[workflow]
strip_globs = [] # empty = falls back to quarantine.DefaultTargets
`

// netScaffold is the [net] section. Values mirror internal/net's own baked
// constants (defaultProbeHost/-Port/-TTLSeconds/-TimeoutMs), so an untouched
// scaffold is a no-op posture.
const netScaffold = `
# ── net: cached internal-network reachability probe (forgectl net) ─────────
[net]
probe_host  = "1.1.1.1" # baked default
probe_port  = 443       # baked default
ttl_seconds = 60        # cached result freshness window, seconds
timeout_ms  = 1000      # probe dial timeout, milliseconds
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
// metrics_dir/runbooks_dir have home-joined baked defaults, but sync.go
// (internal/sessions/sync.go) reads a config-supplied value LITERALLY — it does
// NOT expand ~ the way clean/bench do — so an active `metrics_dir =
// "~/.claude/metrics"` would resolve to a directory named "~" and silently
// break `forgectl sessions sync` (every session skipped, exit 0). They stay
// commented so the absent-key fallback (the correctly home-expanded default)
// applies; the annotation warns anyone who uncomments to use an absolute path.
const sessionsScaffold = `
# ── sessions: cross-machine operational mart ETL (forgectl sessions) ───────
[sessions]
# dsn = "postgres://user@host:5433/sessions_mart" # or $FORGECTL_SESSIONS_DSN; required
# machine = "" # default: short hostname
# metrics_dir  = ""  # default: ~/.claude/metrics — set an ABSOLUTE path (~ is NOT expanded for this key)
# runbooks_dir = ""  # default: ~/.claude/cadence/runbooks — set an ABSOLUTE path (~ is NOT expanded)
`

// reviewScaffold is the [review] section. owners mirrors
// internal/cli/review.go's defaultReviewOwner constant, so an untouched
// scaffold is a no-op posture.
const reviewScaffold = `
# ── review: cross-project work inventory (forgectl review) ─────────────────
[review]
owners = ["cameronsjo"] # gh search --owner scope; baked default when unset
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
// via appendLaunchSection. [launch] reuses launchScaffold (launch_init.go)
// directly rather than a second copy.
var initSections = []initSection{
	{"", "host scalars", hostScalarsScaffold},
	{"launch", "launch", launchScaffold},
	{"workflow", "workflow", workflowScaffold},
	{"net", "net", netScaffold},
	{"bench", "bench", benchScaffold},
	{"docker", "docker", dockerScaffold},
	{"clean", "clean", cleanScaffold},
	{"sessions", "sessions", sessionsScaffold},
	{"review", "review", reviewScaffold},
	{"docs", "docs", docsScaffold},
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
func newInitCmd(module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Scaffold every config.toml section with commented, sensibly-defaulted templates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			slog.Debug("Preparing to scaffold config.toml.")
			path, err := config.ConfigPath()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("read config %s: %w", path, err)
			}

			out := cmd.OutOrStdout()
			added := 0
			for _, s := range initSections {
				if hasSection(data, s.name) {
					fmt.Fprintf(out, "already present: %s\n", s.label)
					continue
				}
				if s.name == "" {
					err = prependHostScalars(path, s.template)
				} else {
					err = appendLaunchSection(path, s.template)
				}
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "added:            %s\n", s.label)
				added++
			}

			slog.Info("Successfully scaffolded config.toml.", "path", path, "sections_added", added)
			fmt.Fprintf(out, "\n%d section(s) added to %s\n", added, path)
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

// prependHostScalars inserts content at the very top of the config.toml at
// path, ahead of any existing bytes, creating the parent directory and the
// file if absent. Host scalars are bare TOML keys with no table header, so
// they parse as document-root only when they precede every [section] in the
// file — appendLaunchSection's append-only write would silently fold them
// into whatever section already precedes the end of the file.
func prependHostScalars(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	if err := os.WriteFile(path, append([]byte(content), existing...), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
