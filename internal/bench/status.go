package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"runtime"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// hearthProject is hearth's frozen compose project name — `docker compose -p
// hearth ps` works from any directory because it queries by this label.
const hearthProject = "hearth"

// chronicleAgent is chronicle's frozen LaunchAgent label.
const chronicleAgent = "local.chronicle-sync"

// State is a component's resolved health — a small closed vocabulary rendered
// with a glyph in the human card and carried verbatim in --json.
type State string

const (
	StateOK            State = "ok"
	StateDegraded      State = "degraded"
	StateUnavailable   State = "unavailable"
	StateNotConfigured State = "not-configured"
)

// Report is the aggregate bench health card. Each component is probed
// independently — one being unavailable never fails the others or the command.
type Report struct {
	Hearth    Component `json:"hearth"`
	Chronicle Component `json:"chronicle"`
	Flux      Component `json:"flux"`
}

// Component is one system's health: an overall State, a one-line human reason,
// and optional per-probe detail lines.
type Component struct {
	Name    string   `json:"name"`
	State   State    `json:"state"`
	Reason  string   `json:"reason"`
	Details []string `json:"details,omitempty"`
}

// Status probes every bench component and returns the aggregate report. It never
// returns an error: an absent dependency (missing docker, unloaded LaunchAgent,
// unconfigured dir) resolves to a named State, not a failure.
func Status(ctx context.Context, cfg config.Config, runner exec.Runner, probe Prober) Report {
	return Report{
		Hearth:    checkHearth(ctx, cfg, runner, probe),
		Chronicle: checkChronicle(ctx, cfg, runner),
		Flux:      checkFlux(ctx, runner),
	}
}

// checkHearth resolves the hearth checkout, queries the compose stack by project
// label, and probes the frozen transport + UIs. Unconfigured dir → not-configured;
// no running containers → unavailable; containers up but a probe fails → degraded.
func checkHearth(ctx context.Context, cfg config.Config, runner exec.Runner, probe Prober) Component {
	c := Component{Name: "hearth"}
	if cfg.Bench.ResolvedHearthDir() == "" {
		c.State = StateNotConfigured
		c.Reason = "set [bench].hearth_dir or $HEARTH_DIR"
		return c
	}

	out, err := runner.Run(ctx, "docker", "compose", "-p", hearthProject, "ps", "--format", "json")
	if err != nil {
		c.State = StateUnavailable
		c.Reason = "docker compose unavailable: " + firstLine(err.Error())
		return c
	}
	total, running, perr := parseComposePS(out)
	if perr != nil {
		c.State = StateDegraded
		c.Reason = "could not parse `docker compose ps` output"
		return c
	}
	if total == 0 {
		c.State = StateUnavailable
		c.Reason = "no hearth containers running (try `forgectl bench up`)"
		return c
	}
	c.Details = append(c.Details, fmt.Sprintf("compose: %d/%d running", running, total))

	// Frozen transport + UIs. Evaluate every probe (no short-circuit) so the card
	// shows the full picture in one pass.
	otlp := otlpTarget(cfg.Bench.ResolvedOTLPEndpoint())
	hearthOK := recordHTTP(ctx, probe, "hearth.localhost", "http://hearth.localhost", &c)
	grafanaOK := recordHTTP(ctx, probe, "grafana.localhost", "http://grafana.localhost", &c)
	otlpOK := recordTCP(ctx, probe, "otlp "+strings.TrimPrefix(otlp, "tcp://"), otlp, &c)

	if running == total && hearthOK && grafanaOK && otlpOK {
		c.State = StateOK
		c.Reason = fmt.Sprintf("%d services up, endpoints reachable", total)
	} else {
		c.State = StateDegraded
		c.Reason = "some hearth checks failed (see details)"
	}
	return c
}

// checkChronicle runs the frozen `status --json` contract (via the checkout's uv
// environment, or a chronicle on PATH) and, on macOS, checks the sync daemon.
func checkChronicle(ctx context.Context, cfg config.Config, runner exec.Runner) Component {
	c := Component{Name: "chronicle"}
	name, args, ok := chronicleStatusCmd(cfg)
	if !ok {
		c.State = StateNotConfigured
		c.Reason = "set [bench].chronicle_dir or $CHRONICLE_DIR (or put chronicle on PATH)"
		return c
	}

	out, err := runner.Run(ctx, name, args...)
	if err != nil {
		c.State = StateUnavailable
		c.Reason = "chronicle status failed: " + firstLine(err.Error())
		return c
	}
	var st ChronicleStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		c.State = StateDegraded
		c.Reason = "chronicle status returned unparseable JSON"
		return c
	}
	c.Details = append(c.Details, fmt.Sprintf("sessions: %d, events: %d, files: %d", st.Sessions, st.Events, st.Files))
	if st.LastSync != nil {
		c.Details = append(c.Details, "last sync: "+*st.LastSync)
	} else {
		c.Details = append(c.Details, "last sync: never")
	}

	c.State = StateOK
	c.Reason = "retention layer reachable"
	// The 5-minute sync daemon is a macOS LaunchAgent; skip the check elsewhere.
	if runtime.GOOS == "darwin" {
		if _, err := runner.Run(ctx, "launchctl", "list", chronicleAgent); err != nil {
			c.State = StateDegraded
			c.Reason = "sync daemon (" + chronicleAgent + ") not loaded"
			c.Details = append(c.Details, "daemon: not loaded")
		} else {
			c.Details = append(c.Details, "daemon: loaded")
		}
	}
	return c
}

// checkFlux is a best-effort board reachability probe: `flux ready` exits 0 when
// the board is reachable. It reports not-configured when the CLI or board env is
// absent (flux ships in dotfiles, not in [bench]).
func checkFlux(ctx context.Context, runner exec.Runner) Component {
	c := Component{Name: "flux"}
	if _, err := osexec.LookPath("flux"); err != nil {
		c.State = StateNotConfigured
		c.Reason = "flux CLI not on PATH"
		return c
	}
	if os.Getenv("FLUX_DIR") == "" && os.Getenv("CADENCE_KANBAN") == "" {
		c.State = StateNotConfigured
		c.Reason = "no board configured ($FLUX_DIR / $CADENCE_KANBAN unset)"
		return c
	}
	if _, err := runner.Run(ctx, "flux", "ready"); err != nil {
		c.State = StateDegraded
		c.Reason = "flux board unreachable: " + firstLine(err.Error())
		return c
	}
	c.State = StateOK
	c.Reason = "board reachable"
	return c
}

// ChronicleStatus is chronicle's frozen `status --json` schema (chronicle
// README, "Interop contract"). A change here is a breaking change on chronicle.
type ChronicleStatus struct {
	Events        int               `json:"events"`
	Files         int               `json:"files"`
	Sessions      int               `json:"sessions"`
	MetricsEvents int               `json:"metrics_events"`
	Sources       []ChronicleSource `json:"sources"`
	LastSync      *string           `json:"last_sync"`
	GeneratedAt   string            `json:"generated_at"`
}

// ChronicleSource is one per-source row in the status object.
type ChronicleSource struct {
	Source string `json:"source"`
	Files  int    `json:"files"`
	Events int    `json:"events"`
}

// chronicleStatusCmd resolves how to invoke chronicle: the checkout's uv
// environment when a dir is configured, else a chronicle on PATH, else none
// (not-configured). Returns (name, args, resolved).
func chronicleStatusCmd(cfg config.Config) (string, []string, bool) {
	if dir := cfg.Bench.ResolvedChronicleDir(); dir != "" {
		return "uv", []string{"--directory", dir, "run", "chronicle", "status", "--json"}, true
	}
	if _, err := osexec.LookPath("chronicle"); err == nil {
		return "chronicle", []string{"status", "--json"}, true
	}
	return "", nil, false
}

// composeContainer is the subset of `docker compose ps --format json` we read.
type composeContainer struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

// parseComposePS counts total and running containers from `docker compose ps
// --format json`, tolerating both the newline-delimited-objects form (current
// compose) and a JSON array (older versions). Empty output is zero containers,
// not an error.
func parseComposePS(out string) (total, running int, err error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return 0, 0, nil
	}
	var containers []composeContainer
	if strings.HasPrefix(out, "[") {
		if err := json.Unmarshal([]byte(out), &containers); err != nil {
			return 0, 0, err
		}
	} else {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var cc composeContainer
			if err := json.Unmarshal([]byte(line), &cc); err != nil {
				return 0, 0, err
			}
			containers = append(containers, cc)
		}
	}
	for _, cc := range containers {
		total++
		if cc.State == "running" {
			running++
		}
	}
	return total, running, nil
}

// otlpTarget turns an OTLP endpoint (http://host:port or host:port) into a
// tcp:// reachability target — the gRPC transport is not HTTP-probeable.
func otlpTarget(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return "tcp://" + u.Host
	}
	return "tcp://" + strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
}

// recordHTTP probes an HTTP URL, appends a detail line, and reports reachability
// (any HTTP response counts — the server answered).
func recordHTTP(ctx context.Context, probe Prober, label, target string, c *Component) bool {
	code, err := probe.Probe(ctx, target)
	if err != nil {
		c.Details = append(c.Details, label+": unreachable")
		return false
	}
	c.Details = append(c.Details, fmt.Sprintf("%s: %d", label, code))
	return true
}

// recordTCP probes a tcp:// target, appends a detail line, and reports whether
// the connect succeeded.
func recordTCP(ctx context.Context, probe Prober, label, target string, c *Component) bool {
	if _, err := probe.Probe(ctx, target); err != nil {
		c.Details = append(c.Details, label+": unreachable")
		return false
	}
	c.Details = append(c.Details, label+": reachable")
	return true
}

// firstLine trims a possibly multi-line error string to its first line so a
// component reason stays a single tidy line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
