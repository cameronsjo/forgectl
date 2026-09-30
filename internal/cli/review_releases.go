package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/review"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Environment overrides for `review releases` (tool-prefixed, per the
// estate's env-var rule). The flags win over these.
const (
	envReleaseRegistry = "FORGECTL_RELEASE_REGISTRY"
	envGateSHA256      = "FORGECTL_GATE_SHA256"
)

// defaultRegistryRel is the registry's home under $HOME: the
// cadence-ecosystem checkout's docs/release-rhythm.yaml.
const defaultRegistryRel = "Projects/cadence-ecosystem/docs/release-rhythm.yaml"

// canonicalGateRel is where the canonical gate sits relative to the
// registry's directory (docs/ → scripts/release/ship-gate.sh).
const canonicalGateRel = "../scripts/release/ship-gate.sh"

var reHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// errReleasesStalled is the --fail-on-stall exit: the report was printed,
// and at least one row is stalled or unknown.
var errReleasesStalled = errors.New("release radar: stalled or unknown repos")

// newReviewReleasesCmd builds `forgectl review releases` over api and a
// clock — the test seam.
func newReviewReleasesCmd(api review.APIGetter, now func() time.Time) *cobra.Command {
	var (
		registry    string
		gateSHA     string
		asJSON      bool
		failOnStall bool
	)
	cmd := &cobra.Command{
		Use:   "releases [--registry <path>] [--json] [--fail-on-stall]",
		Short: "Release radar: what each rhythm repo shipped, what waits, and what stalled",
		Long: `releases reads the release-rhythm registry (cadence-ecosystem
docs/release-rhythm.yaml, ADR-0044) and asks GitHub, read-only, where every
release-pr, testflight, and manual-cut repo stands: its last release, commits
since, the release PR waiting, the last ship run and the gate's reason, human
gates, endpoint lag (tap and bucket versions), and whether the vendored
ship-gate.sh still matches the canonical copy. Continuous and dormant repos
are listed in a footer.

A repo is stalled when its nightly toggle is on and it has no ship run in 26h,
when the same non-quiet gate reason (not go, no-pr, or paused) shows on its
last 2 scheduled runs, when the last run reports half-shipped, when an
endpoint trails the release by more than 24h, or when its gate copy drifted.
A paused repo (toggle not on) is shown but not judged on its beat. A repo
whose reads failed is unknown, and the failure is named.

  forgectl review releases                    table
  forgectl review releases --json             machine-readable report
  forgectl review releases --fail-on-stall    exit 1 on any stalled or unknown repo

The registry path comes from --registry, then $` + envReleaseRegistry + `, then
~/` + defaultRegistryRel + `. The canonical gate hash comes from --gate-sha256,
then $` + envGateSHA256 + `, then the sha256 of ` + canonicalGateRel + ` beside the
registry. GitHub auth is gh's: GH_TOKEN, GITHUB_TOKEN, or its stored login.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			regPath, err := resolveRegistryPath(registry)
			if err != nil {
				return err
			}
			reg, err := review.LoadRegistry(regPath)
			if err != nil {
				return err
			}
			canonical, err := resolveCanonicalGate(gateSHA, regPath)
			if err != nil {
				return err
			}
			facts := review.Collect(cmd.Context(), api, reg)
			rep := review.BuildReport(reg, facts, canonical, now())
			if asJSON {
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else if err := renderReleasesTable(cmd.OutOrStdout(), rep); err != nil {
				return err
			}
			if failOnStall && rep.Failing() {
				return errReleasesStalled
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "path to release-rhythm.yaml (default $"+envReleaseRegistry+" or ~/"+defaultRegistryRel+")")
	cmd.Flags().StringVar(&gateSHA, "gate-sha256", "", "canonical ship-gate.sh sha256 (default $"+envGateSHA256+" or the file beside the registry)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON to stdout")
	cmd.Flags().BoolVar(&failOnStall, "fail-on-stall", false, "exit 1 when any repo is stalled or unknown")
	return cmd
}

func resolveRegistryPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(envReleaseRegistry); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve default registry path: %w (pass --registry)", err)
	}
	return filepath.Join(home, defaultRegistryRel), nil
}

// resolveCanonicalGate returns the canonical gate hash, or "" when none is
// available. An empty hash does not fail the command: each release-pr row
// then reports its gate copy as unknowable, which --fail-on-stall treats as
// a failure.
func resolveCanonicalGate(flag, regPath string) (string, error) {
	for _, v := range []string{flag, os.Getenv(envGateSHA256)} {
		if v == "" {
			continue
		}
		v = strings.ToLower(strings.TrimSpace(v))
		if !reHex64.MatchString(v) {
			return "", errors.New("canonical gate sha256 must be 64 hex characters")
		}
		return v, nil
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(regPath), canonicalGateRel))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read canonical gate: %w", err)
	}
	return review.SHA256Hex(raw), nil
}

// renderReleasesTable writes the radar table, then one line per stall or
// error, then the untracked footer. Every API-derived string passes through
// safeTerm: tags, versions, and reasons come from repos, not from us.
func renderReleasesTable(out io.Writer, rep review.Report) error {
	now := rep.GeneratedAt
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	w := &stickyWriter{w: tw}
	w.printf("REPO\tCLASS\tSTATE\tLAST RELEASE\tUNRELEASED\tRELEASE PR\tLAST SHIP\tHUMAN GATES\tENDPOINTS\tGATE COPY\n")
	for _, r := range rep.Rows {
		w.printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Repo, r.Class, r.State,
			safeTerm(releaseCell(r, now)),
			unreleasedCell(r),
			prCell(r, now),
			safeTerm(shipCell(r, now)),
			gatesCell(r, now),
			safeTerm(endpointsCell(r)),
			dash(r.GateCopy))
	}
	if w.err != nil {
		return w.err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	w = &stickyWriter{w: &buf}
	var notes []string
	for _, r := range rep.Rows {
		for _, st := range r.Stalls {
			notes = append(notes, "stall   "+r.Repo+": "+safeTerm(st))
		}
		for _, e := range r.Errors {
			notes = append(notes, "unknown "+r.Repo+": "+safeTerm(e))
		}
	}
	if len(notes) > 0 {
		w.printf("\n%s\n", strings.Join(notes, "\n"))
	}
	if len(rep.Footer) > 0 {
		parts := make([]string, 0, len(rep.Footer))
		for _, f := range rep.Footer {
			p := f.Repo + " (" + f.Class
			if len(f.HumanGates) > 0 {
				p += "; " + strings.Join(f.HumanGates, "; ")
			}
			parts = append(parts, p+")")
		}
		w.printf("\nnot tracked: %s\n", safeTerm(strings.Join(parts, ", ")))
	}
	if w.err != nil {
		return w.err
	}
	_, err := out.Write(buf.Bytes())
	return err
}

// stickyWriter keeps the first write error so a long render checks once.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) printf(format string, a ...any) {
	if s.err == nil {
		_, s.err = fmt.Fprintf(s.w, format, a...)
	}
}

func releaseCell(r review.Row, now time.Time) string {
	if r.LastReleaseAt == nil {
		return "none"
	}
	return fmt.Sprintf("%s (%s)", r.LastRelease, review.Age(now, *r.LastReleaseAt))
}

func unreleasedCell(r review.Row) string {
	if r.Unreleased == nil {
		return "-"
	}
	return fmt.Sprint(*r.Unreleased)
}

func prCell(r review.Row, now time.Time) string {
	if r.ReleasePR == nil {
		return "-"
	}
	s := fmt.Sprintf("#%d (%s)", r.ReleasePR.Number, review.Age(now, r.ReleasePR.CreatedAt))
	if r.ReleasePRCount > 1 {
		s += fmt.Sprintf(" +%d", r.ReleasePRCount-1)
	}
	return s
}

func shipCell(r review.Row, now time.Time) string {
	if r.LastRun == nil {
		return "-"
	}
	run := r.LastRun
	outcome := run.Conclusion
	if run.Status != "completed" {
		outcome = run.Status
	}
	s := fmt.Sprintf("%s ago %s", review.Age(now, run.CreatedAt), outcome)
	if run.Reason != "" {
		s += " " + run.Reason
	}
	return s
}

func gatesCell(r review.Row, now time.Time) string {
	if len(r.HumanGates) == 0 {
		return "-"
	}
	if r.Class == review.ClassTestflight {
		// App Store release state is in App Store Connect, not on GitHub.
		return fmt.Sprintf("%d, age n/a", len(r.HumanGates))
	}
	if r.HumanGateDueSince != nil {
		return fmt.Sprintf("%d, due %s", len(r.HumanGates), review.Age(now, *r.HumanGateDueSince))
	}
	return fmt.Sprintf("%d, none due", len(r.HumanGates))
}

func endpointsCell(r review.Row) string {
	if len(r.Endpoints) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(r.Endpoints))
	for _, ep := range r.Endpoints {
		if ep.Kind == "testflight" {
			parts = append(parts, "testflight")
			continue
		}
		p := ep.Kind + " " + dash(ep.Version)
		if ep.Behind {
			p += " behind"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
