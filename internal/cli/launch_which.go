package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func newLaunchWhichCmd(boundary *config.LegacyMigrationBoundary, cfg config.Config, th theme.Theme) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "which",
		Short: "Print the resolved launch profile for the current directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := launchWorkingDirectory()
			if err != nil {
				return err
			}
			effLaunch, notice, effFrom := autoMigrateOrWarnLegacyLaunch(boundary, cfg)
			if notice != "" && !asJSON {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "forgectl: "+safeText(notice))
			}
			cfg.Launch = effLaunch
			lc, source := resolveLaunchConfigSource(boundary, cfg, effFrom)
			src := source.label()
			profile, err := launch.Resolve(lc, cwd)
			if err != nil {
				return WithExitCode(termsafe.Error(err), exitUsage)
			}
			// The injected block is not part of the profile, so without this
			// `which` reports a posture that omits variables the launch will
			// carry — and a bad [proxy] launch_profile printed as a clean
			// posture that every launch refuses. Exit 2 matches the launch
			// paths: a read-only verb must not disagree with them about
			// whether this config can launch.
			injected, err := injectedLaunchKeys(cfg)
			if err != nil {
				return WithExitCode(termsafe.Error(err), exitUsage)
			}
			// Where a bare `forgectl launch` would start the session: only a
			// claude session moves to the repository's settings root, so
			// every other harness reports none.
			runDir := ""
			if profile.Harness == "claude" {
				runDir = launch.SettingsRoot(cwd)
			}
			// What a worker started here would get (forgectl#1125): a worker
			// does not read [launch.defaults], so the profile above is not
			// its posture. Resolved through the launch's own worker path.
			worker, err := launch.ResolveWorkerPosture(lc, cwd)
			if err != nil {
				return WithExitCode(termsafe.Error(err), exitUsage)
			}
			if asJSON {
				doc := buildLaunchWhichJSON(profile, cwd, runDir, source, injected)
				doc.Worker = buildLaunchWhichWorkerJSON(worker)
				return encodeLaunchWhichJSON(cmd.OutOrStdout(), doc)
			}
			out := th.Writer(cmd.OutOrStdout(), os.Environ())
			printLaunchProfile(out, th, profile, cwd, runDir, src, injected)
			printWorkerPosture(out, th, worker)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"directory":...,"run_directory":...,"config":...,"config_path":...,"config_note":...,"matched":...,"harness":...,"model":...,"effort":...,"permission_mode":...,"allow_danger":...,"env_keys":[...],"injected_env_keys":[...],"add_dir":[...]} to stdout`)
	return cmd
}

// launchWhichJSON is the --json wire shape for `launch which`. Env is
// represented only by its sorted key NAMES — the same key-names-only rule
// printLaunchProfile's terminal row applies, because `which` output (human or
// machine) is the kind of thing pasted into an issue or an agent transcript,
// and a configured env value is exactly where a secret lives.
type launchWhichJSON struct {
	Directory string `json:"directory"`
	// RunDirectory is where a bare `forgectl launch` starts the session: the
	// repository's settings root for a claude launch from a subfolder,
	// otherwise Directory. Present for the claude harness only, the one that
	// can move (launch.SettingsRoot).
	RunDirectory string `json:"run_directory,omitempty"`
	// Config is the human source label: the path, then any note in
	// parentheses. ConfigPath and ConfigNote carry the two apart.
	Config string `json:"config"`
	// ConfigPath is the file the profile was read from, or would be (a
	// missing config.toml), usable as a path.
	ConfigPath string `json:"config_path"`
	// ConfigNote is "" for the file's own [launch] section, else why it is
	// not: "legacy", "missing — built-in defaults", and the like.
	ConfigNote     string   `json:"config_note"`
	Matched        string   `json:"matched"`
	Harness        string   `json:"harness"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	PermissionMode string   `json:"permission_mode"`
	AllowDanger    bool     `json:"allow_danger"`
	EnvKeys        []string `json:"env_keys"`
	// InjectedEnvKeys names the variables forgectl itself adds to the harness
	// environment — the bench telemetry block and the [proxy] launch_profile.
	// Names only, same rule as EnvKeys: this answers "will my proxy be
	// applied?", which no verb could answer before, without putting a proxy
	// URL on a surface that gets pasted into an issue.
	InjectedEnvKeys []string `json:"injected_env_keys"`
	AddDir          []string `json:"add_dir"`
	// Worker is the posture a worker started in Directory gets: a worker
	// reads [launch.worker], not [launch.defaults], so the fields above are
	// not its posture (forgectl#1125).
	Worker *launchWhichWorkerJSON `json:"worker,omitempty"`
}

// launchWhichWorkerJSON is the effective worker posture. Posture names each
// field the harness takes with its source: "default" (the built-in worker
// value), "launch.worker", or "repo-profile" (the matched project block's
// stricter value). Refused is the worker floor's refusal, empty when a worker
// may start here.
type launchWhichWorkerJSON struct {
	Harness string                   `json:"harness"`
	Posture []launchWhichPostureJSON `json:"posture"`
	Notes   []string                 `json:"notes"`
	Refused string                   `json:"refused,omitempty"`
}

type launchWhichPostureJSON struct {
	Field  string `json:"field"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

func buildLaunchWhichWorkerJSON(w launch.WorkerPosture) *launchWhichWorkerJSON {
	out := &launchWhichWorkerJSON{Harness: w.Harness, Posture: []launchWhichPostureJSON{}, Notes: []string{}, Refused: w.Refused}
	for _, v := range w.Values {
		out.Posture = append(out.Posture, launchWhichPostureJSON(v))
	}
	if w.Notes != nil {
		out.Notes = w.Notes
	}
	return out
}

// workerPostureSummary is the one-line worker posture: the harness, then each
// field with its value and source.
func workerPostureSummary(w *launchWhichWorkerJSON) string {
	parts := make([]string, 0, len(w.Posture))
	for _, v := range w.Posture {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", v.Field, v.Value, v.Source))
	}
	switch {
	case len(parts) > 0:
		return w.Harness + "  " + strings.Join(parts, ", ")
	case w.Refused == "":
		return w.Harness + "  (no posture flags)"
	}
	return w.Harness
}

// printWorkerPosture renders the worker row under the launch profile.
func printWorkerPosture(w io.Writer, th theme.Theme, p launch.WorkerPosture) {
	styles := th.Styles()
	labelStyle := styles.Muted.Width(14)
	valueStyle := styles.Fg
	dimStyle := styles.Muted.Italic(true)
	row := func(label, value string, style func(...string) string) {
		_, _ = fmt.Fprintln(w, renderSafe(labelStyle.Render, label)+renderSafe(style, value))
	}

	view := buildLaunchWhichWorkerJSON(p)
	row("worker", workerPostureSummary(view), valueStyle.Render)
	if view.Refused != "" {
		row("", "refused: "+view.Refused, dimStyle.Render)
	}
	for _, n := range view.Notes {
		row("", "note: "+n, dimStyle.Render)
	}
}

// buildLaunchWhichJSON converts a resolved profile into the --json wire
// shape. Slice fields are never nil so the encoder emits [] rather than null
// for a profile with no env or no add-dir entries.
func buildLaunchWhichJSON(p launch.Profile, cwd, runDir string, src launchConfigSource, injected []string) launchWhichJSON {
	envKeys := launch.SortedEnvKeys(p.Env)
	if envKeys == nil {
		envKeys = []string{}
	}
	if injected == nil {
		injected = []string{}
	}
	addDir := p.AddDir
	if addDir == nil {
		addDir = []string{}
	}
	return launchWhichJSON{
		Directory:       cwd,
		RunDirectory:    runDir,
		Config:          src.label(),
		ConfigPath:      src.Path,
		ConfigNote:      src.Note,
		Matched:         p.Match,
		Harness:         p.Harness,
		Model:           p.Model,
		Effort:          p.Effort,
		PermissionMode:  p.PermissionMode,
		AllowDanger:     p.AllowDanger,
		EnvKeys:         envKeys,
		InjectedEnvKeys: injected,
		AddDir:          addDir,
	}
}

// encodeLaunchWhichJSON encodes a built document through the sanctioned
// termsafe seam. Nothing is written before a marshal error, so a failing
// writer or encoder never leaves a partial document on stdout.
func encodeLaunchWhichJSON(w io.Writer, doc launchWhichJSON) error {
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func printLaunchProfile(w io.Writer, th theme.Theme, p launch.Profile, cwd, runDir, confPath string, injected []string) {
	styles := th.Styles()
	labelStyle := styles.Muted.Width(14)
	valueStyle := styles.Fg
	titleStyle := styles.Accent
	dimStyle := styles.Muted.Italic(true)

	row := func(label, value string) {
		_, _ = fmt.Fprintln(w, renderSafe(labelStyle.Render, label)+renderSafe(valueStyle.Render, value))
	}
	rowDim := func(label, value string) {
		_, _ = fmt.Fprintln(w, renderSafe(labelStyle.Render, label)+renderSafe(dimStyle.Render, value))
	}

	_, _ = fmt.Fprintln(w, titleStyle.Render("launch profile")+renderSafe(dimStyle.Render, "  "+cwd))
	row("config", confPath)
	if runDir != "" && runDir != cwd {
		row("runs in", runDir+"  (settings root; --here to stay)")
	}

	matched := p.Match
	if matched == "" {
		rowDim("matched", "(defaults only)")
	} else {
		row("matched", matched)
	}
	row("harness", p.Harness)
	if p.Harness == "pi" && p.Provider != "" {
		row("provider", p.Provider)
	}
	row("model", p.Model)
	switch p.Harness {
	case "codex":
		row("approval", p.ApprovalPolicy)
		row("sandbox", p.Sandbox)
	case "claude":
		// Omitted rather than shown empty when no level resolved: an absent
		// --effort means Claude Code's own default applies, which is a
		// different statement from "effort is blank". Matches the conditional
		// env/add-dir rows below.
		if p.Effort != "" {
			row("effort", p.Effort)
		}
		row("permission", p.PermissionMode)
		row("allow danger", fmt.Sprintf("%t", p.AllowDanger))
	}

	// Env renders its sorted KEY NAMES only, never its values: this is arbitrary
	// environment injected into the launched harness, so it is where an
	// ANTHROPIC_API_KEY or GH_TOKEN sits, and `which` output is the kind of
	// thing pasted into an issue or a terminal share. The key names carry the
	// signal an operator needs here — which variables the profile injects.
	// Same policy, same rendering as the sibling surface: leafValue's
	// reflect.Map arm in internal/cli/config_cmd.go.
	if len(p.Env) > 0 {
		row("env", redactedMapDisplay(launch.SortedEnvKeys(p.Env)))
	}
	// Injected is a separate row from env on purpose: one is what the operator
	// configured for this directory, the other is what forgectl adds on top,
	// and collapsing them would hide which is which.
	if len(injected) > 0 {
		row("injected", redactedMapDisplay(injected))
	}
	for i, d := range p.AddDir {
		label := ""
		if i == 0 {
			label = "add-dir"
		}
		row(label, d)
	}
}

// renderSafe establishes the ordering invariant for styled terminal output:
// untrusted text is escaped first, then the trusted renderer may add ANSI.
func renderSafe(render func(...string) string, untrusted string) string {
	return render(safeText(untrusted))
}

// launchGetwd is os.Getwd, a variable only so a test can make it fail.
var launchGetwd = os.Getwd

// launchWorkingDirectory is the working directory `launch` and `launch which`
// resolve a profile for. Its *PathError goes through termsafe.Error BEFORE the
// wrap (#832): termsafe.Error reconstructs and caps the path only for a
// *PathError that is the error itself, so wrapping first rendered the path
// escaped but at full length.
func launchWorkingDirectory() (string, error) {
	cwd, err := launchGetwd()
	if err != nil {
		return "", fmt.Errorf("determine working directory: %w", termsafe.Error(err))
	}
	return cwd, nil
}
