package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/doctor"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func newLaunchDoctorCmd(boundary *config.LegacyMigrationBoundary, cfg config.Config, th theme.Theme) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check harness availability and launch config validity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Under --json the checks are collected rather than printed, and
			// stdout is the raw writer, never th.Writer: a machine payload
			// must not pass through the colour writer (doctor.go's split).
			rec := &launchDoctorRecorder{marks: th.Marks(), asJSON: asJSON}
			if !asJSON {
				rec.out = th.Writer(cmd.OutOrStdout(), os.Environ())
			}
			healthy := true

			// Same rule as launchExec: the opt-in is whatever config.toml
			// declared at process start, never what a migration produced.
			usageEnabled := cfg.Launch.UsageStats

			effLaunch, notice, effFrom := autoMigrateOrWarnLegacyLaunch(boundary, cfg)
			cfg.Launch = effLaunch

			lc, src := resolveLaunchConfig(boundary, cfg, effFrom)

			profile := launch.DefaultsProfile(lc)
			if cwd, err := os.Getwd(); err == nil {
				profile = launch.Resolve(lc, cwd)
			}
			if err := profile.Validate(); err != nil {
				healthy = false
				rec.add("profile", doctor.StateFail, "launch profile invalid: "+safeText(err.Error()))
			}
			// [pr] effort is validated separately because it never enters the
			// resolved [launch] profile above — it is applied inside the review
			// dispatch. Without this, the one setting whose failure is INVISIBLE
			// at runtime (a review runs in a detached tmux window, so a value
			// claude rejects is an empty pane and no error) is also the one
			// doctor cannot pre-flight. Harness is pinned to claude because the
			// review dispatch forces it there regardless of the ambient profile.
			if cfg.Pr.Effort != "" {
				if err := (launch.Profile{Harness: "claude", Effort: cfg.Pr.Effort}).Validate(); err != nil {
					healthy = false
					rec.add("pr_config", doctor.StateFail, "[pr] config invalid: "+safeText(err.Error()))
				}
			}
			resolvedBinary, binaryErr := launch.ResolveBinary(profile.Harness, lc.Defaults)
			binaryPath := resolvedBinary.Path
			if binaryErr == nil {
				rec.add("harness", doctor.StateOK, safeLabel(profile.Harness)+" found: "+termsafe.QuotePath(binaryPath))
			} else {
				healthy = false
				rec.add("harness", doctor.StateFail, safeText(binaryErr.Error()))
			}

			configPath := ""
			if boundary != nil {
				configPath = boundary.ConfigPath
			} else {
				configPath, _ = config.ConfigPath()
			}
			var parseErr error
			if boundary == nil || !errors.Is(boundary.Refusal, config.ErrLegacyPathControl) {
				parseErr = config.ValidatePath(configPath)
			}
			// The notice prints ahead of the switch, not inside one arm. A
			// legacy file forgectl models nothing of leaves lc zero, which
			// takes the "no launch profiles configured" arm — so gating the
			// notice on the default arm silently dropped it for the exact
			// input class #417 is about (#418 review).
			if notice != "" {
				rec.add("legacy_migration", doctor.StateWarn, safeText(notice))
			}
			switch {
			case parseErr != nil:
				healthy = false
				rec.add("config", doctor.StateFail, "config failed to parse: "+safeText(parseErr.Error()))
			case !cfg.HasLaunchSection() && lc.IsZero():
				var legacyErr error
				if boundary != nil && boundary.Status != config.BoundaryNoSource {
					_, legacyErr = boundary.LoadReadOnlyLegacy()
				}
				if legacyErr != nil {
					healthy = false
					rec.add("config", doctor.StateFail, "legacy claunch config failed to parse: "+safeText(legacyErr.Error()))
				} else {
					rec.add("config", doctor.StateWarn, "no launch profiles configured — using built-in defaults (run `forgectl launch init`)")
				}
				// #417: name a config file in the legacy directory that
				// forgectl cannot migrate, so "no profiles configured" does
				// not read as "nothing is there".
				if sibling := boundary.UnmigratableSiblingPath(); sibling != "" {
					rec.add("config", doctor.StateWarn, termsafe.QuotePath(sibling)+
						" is present but forgectl cannot migrate it — it migrates the historical claunch.conf format only")
				}
			default:
				rec.add("config", doctor.StateOK, fmt.Sprintf("launch config: %s (%d project profile(s))", termsafe.QuotePath(src), len(lc.Projects)))
			}

			if !rec.usageStats(usageEnabled) {
				healthy = false
			}

			// Bench telemetry injection is informational, not a health signal —
			// off is a valid choice (a machine with no local collector).
			if cfg.Bench.Telemetry {
				rec.add("telemetry", doctor.StateOK, "telemetry: on → "+
					safeText(endpointForDisplay(cfg.Bench.ResolvedOTLPEndpoint()))+" ("+safeLabel(cfg.Bench.ResolvedOTLPProtocol())+")")
			} else {
				rec.add("telemetry", doctor.StateWarn, "telemetry: off (enable with [bench].telemetry = true)")
			}

			// The same check `forgectl pr` runs before it opens a review window,
			// so doctor is never green for a config pr would refuse. The
			// refusal names the setting, never its value.
			if _, err := injectedWindowEnv(cfg); err != nil {
				rec.add("review_window_env", doctor.StateFail, "review-window environment: "+safeText(err.Error()))
				healthy = false
			}

			// The update-hooks watcher is optional, so this row warns and
			// never fails the doctor. It reads only: a plist stat,
			// `launchctl print`, and forgectl's own hook state files.
			if hooksGOOS == "darwin" {
				ctx := cmd.Context()
				if ctx == nil {
					ctx = context.Background()
				}
				cfgHooks, cfgErr := cfg.ResumeHooks()
				facts, probeErr := hooksDoctorProbe(ctx)
				state, detail := hooksDoctorRow(len(cfgHooks), hooksLongestTimeout(cfgHooks), cfgErr, facts, probeErr)
				rec.add("update_hooks", state, detail)
			}

			if asJSON {
				if err := writeLaunchDoctorJSON(cmd.OutOrStdout(), rec.checks, healthy); err != nil {
					return err
				}
			}
			if !healthy {
				// Under --json the checks on stdout are the verdict
				// (forgectl#862).
				return jsonVerdict(fmt.Errorf("doctor found problems"), asJSON)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"checks":[{"name":...,"state":"ok|warn|fail","detail":...}],"healthy":...} to stdout`)
	return cmd
}

// launchDoctorCheckJSON is one `launch doctor --json` check — the same line
// the human output prints, split into the check it belongs to, its state (the
// glyph, as doctor.State's ok/warn/fail vocabulary), and its text. Detail is
// the human line verbatim, so every redaction the terminal applies — the OTLP
// endpoint's hidden query and userinfo via endpointForDisplay, key names never
// values — applies here by construction; there is no second rendering to
// drift. Name is one of profile, pr_config, harness, legacy_migration, config,
// usage_stats, telemetry, review_window_env, update_hooks, and may repeat.
type launchDoctorCheckJSON struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// launchDoctorJSON is `launch doctor --json`'s stdout wire shape, mirroring
// `forgectl doctor --json` (doctorReportJSON). Healthy is false exactly when
// the command exits non-zero.
type launchDoctorJSON struct {
	Checks  []launchDoctorCheckJSON `json:"checks"`
	Healthy bool                    `json:"healthy"`
}

// writeLaunchDoctorJSON encodes the collected checks through the sanctioned
// termsafe seam. An empty check list encodes [], never null.
func writeLaunchDoctorJSON(w io.Writer, checks []launchDoctorCheckJSON, healthy bool) error {
	if checks == nil {
		checks = []launchDoctorCheckJSON{}
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(launchDoctorJSON{Checks: checks, Healthy: healthy})
}

// launchDoctorRecorder is the one sink every doctor line goes through: on the
// human path it prints "<mark> <detail>" immediately, byte-identical to the
// output before --json existed; under --json it collects the line instead.
type launchDoctorRecorder struct {
	out    io.Writer
	marks  theme.Marks
	asJSON bool
	checks []launchDoctorCheckJSON
}

func (r *launchDoctorRecorder) add(name string, state doctor.State, detail string) {
	if r.asJSON {
		r.checks = append(r.checks, launchDoctorCheckJSON{Name: name, State: string(state), Detail: detail})
		return
	}
	_, _ = fmt.Fprintf(r.out, "%s %s\n", doctorMark(state, r.marks), detail)
}

// usageStats runs the usage-statistics check through the recorder, so both
// paths get the same lines from one source.
func (r *launchDoctorRecorder) usageStats(enabled bool) bool {
	return collectUsageStats(enabled, func(state doctor.State, detail string) {
		r.add("usage_stats", state, detail)
	})
}

// endpointForDisplay hides the parts of an endpoint that carry secrets before
// it is printed: the query string, where an ingest key rides, and any
// user:pass@ userinfo. `forgectl pr` refuses both for that reason; doctor must
// not print the secret it is about to report as refused.
func endpointForDisplay(endpoint string) string {
	if i := strings.IndexByte(endpoint, '?'); i >= 0 {
		endpoint = endpoint[:i] + "?[query hidden]"
	}
	start := 0
	if i := strings.Index(endpoint, "://"); i >= 0 {
		start = i + len("://")
	}
	end := len(endpoint)
	if i := strings.IndexAny(endpoint[start:], "/?#"); i >= 0 {
		end = start + i
	}
	if at := strings.LastIndexByte(endpoint[start:end], '@'); at >= 0 {
		endpoint = endpoint[:start] + "[userinfo hidden]" + endpoint[start+at:]
	}
	return endpoint
}
