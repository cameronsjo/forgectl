package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

// resumeModule declares the cross-project session-resume extension (ADR-0005).
// It owns no config section: the picker is zero-configuration by design, and
// the posture it resumes into comes from [launch], which launchModule owns.
var resumeModule = module.Manifest{
	Name:      "resume",
	Tier:      module.TierExtension,
	ConfigKey: "",
	New:       newResumeCmd,
}

// newResumeCmd builds `forgectl resume` over the registry Deps.
func newResumeCmd(deps module.Deps) *cobra.Command {
	var fork bool
	var limit int

	cmd := &cobra.Command{
		Use:   "resume [filter]",
		Short: "Pick a recent Claude Code session in any repo and resume it there",
		Long: `resume lists every recent Claude Code session across every repo — name,
repo, branch, last activity — and lands you back inside the one you pick, in
the right directory, with its task list restored.

It exists because a terminal restart costs three steps otherwise: find the
folder, run claude --resume, then recognize the session in a picker that shows
neither repo nor branch.

  forgectl resume              pick from the recent sessions
  forgectl resume forgectl     filter by repo, name, cwd, or id; one hit resumes
  forgectl resume ls --json    list without acting
  forgectl resume snapshot     capture what a session's exit would destroy

Session identity is read, never persisted — the prompt history and per-project
transcripts already hold the id, cwd, branch, and title indefinitely. Only two
things need capturing, and ` + "`resume snapshot`" + ` is what captures them: the
/rename name, which lives only in the live-process registry, and the task
bodies, which Claude Code deletes when a session ends. Wire it to a Stop hook
so every turn refreshes the snapshot:

  {"hooks":{"Stop":[{"hooks":[
    {"type":"command","command":"forgectl resume snapshot --quiet"}]}]}}

A session whose process is still running cannot be CONTINUED — two processes on
one transcript corrupts it — so resume refuses and names the live pid. --fork
gets you in anyway: it branches a new session off the transcript, which only
reads it. A forked session starts its own task list, so snapshotted tasks are
reported rather than restored.

Exit codes: 0 resumed (or listed), 1 no session matched, the pick was
cancelled, or the target is still live.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			filter := ""
			if len(args) == 1 {
				filter = args[0]
			}
			return runResume(cmd, deps.Cfg, filter, limit, fork)
		},
	}
	cmd.Flags().BoolVar(&fork, "fork", false, "branch a new session off the transcript instead of continuing it")
	cmd.Flags().IntVar(&limit, "limit", resume.DefaultLimit, "how many recent sessions to consider")

	cmd.AddCommand(newResumeLsCmd(), newResumeSnapshotCmd())
	return cmd
}

// resumePaths is the location seam for every resume subcommand — a
// package-level var so a test can point the whole verb at a fixture tree
// (mirrors upgradeLookPath's seam pattern).
//
// It is not a convenience. `resume snapshot` WRITES, and without a seam its
// test would run a real capture against the developer's own ~/.claude and
// forgectl store — a unit test with production side effects on every machine
// that runs `go test`.
var resumePaths = resume.DefaultPaths

// scanFor loads the resumable sessions matching filter. Live sessions are
// included deliberately: hiding one would turn "you cannot resume this, it is
// still running as pid N" into a silent "no session matched", which sends you
// hunting for a session that is right there.
func scanFor(filter string, limit int) ([]resume.Session, error) {
	paths, err := resumePaths()
	if err != nil {
		return nil, fmt.Errorf("locate session records: %w", err)
	}
	return resume.Scan(paths, resume.Opts{Filter: filter, Limit: limit, IncludeLive: true})
}

// runResume is the pick-then-resume path.
func runResume(cmd *cobra.Command, cfg config.Config, filter string, limit int, fork bool) error {
	sessions, err := scanFor(filter, limit)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		if filter != "" {
			return WithExitCode(fmt.Errorf("no session matched %q — try `forgectl resume ls` to see what's there", sanitizeTerm(filter)), 1)
		}
		return WithExitCode(fmt.Errorf("no recent sessions found"), 1)
	}

	picked := sessions[0]
	if len(sessions) > 1 {
		if picked, err = pickSession(sessions); err != nil {
			return WithExitCode(err, 1)
		}
	}
	return resumeSession(cmd, cfg, picked, fork)
}

// pickSession runs the single-select. Options are keyed by session id so a
// selection round-trips unambiguously (the same reason pickPRs keys on a ref).
func pickSession(sessions []resume.Session) (resume.Session, error) {
	opts := make([]huh.Option[string], len(sessions))
	for i, s := range sessions {
		opts[i] = huh.NewOption(sessionRow(s), s.ID)
	}
	var chosen string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Recent sessions — enter to resume").
				Options(opts...).
				Value(&chosen),
		),
	).Run()
	if err != nil {
		return resume.Session{}, err
	}
	for _, s := range sessions {
		if s.ID == chosen {
			return s, nil
		}
	}
	return resume.Session{}, fmt.Errorf("no session selected")
}

// sessionRow renders one picker row. Every field is disk-sourced, so every
// field goes through sanitizeTerm.
func sessionRow(s resume.Session) string {
	name := s.Name
	if name == "" {
		name = s.ID
	}
	row := fmt.Sprintf("%s %s %s %s",
		cell(sanitizeTerm(name), 28),
		cell(sanitizeTerm(s.Repo), 22),
		cell(sanitizeTerm(s.Branch), 18),
		relativeTime(s.LastActive))
	switch {
	case s.Live:
		row += fmt.Sprintf("  (running, pid %d)", s.Pid)
	case len(s.Tasks) > 0:
		row += fmt.Sprintf("  (%d tasks)", len(s.Tasks))
	}
	return row
}

// resumeSession restores the session's tasks, moves into its directory, and
// replaces this process with claude.
func resumeSession(cmd *cobra.Command, cfg config.Config, s resume.Session, fork bool) error {
	errOut := cmd.ErrOrStderr()

	// The one failure mode this feature introduces. Two claude processes
	// CONTINUING one transcript is corruption, not a resume — so it is
	// refused rather than warned about, and the pid is named so the session
	// is findable. --fork is the documented way past it, and it is a real
	// escape rather than a suggestion: a fork only READS the transcript and
	// writes to a new session of its own, so it does not contend.
	if s.Live && !fork {
		return WithExitCode(fmt.Errorf(
			"session %s is still running as pid %d — continuing it a second time would corrupt the transcript; switch to that terminal, or pass --fork to branch a new session off it",
			sanitizeTerm(s.ID), s.Pid), 1)
	}
	if s.Cwd == "" {
		return WithExitCode(fmt.Errorf("session %s has no recorded working directory to resume into", sanitizeTerm(s.ID)), 1)
	}

	// Tasks go back BEFORE the exec: once syscall.Exec replaces this
	// process there is no "after".
	//
	// Not on a fork, though, and the reason is structural rather than
	// cautious: a fork starts a NEW session id, so it reads a task directory
	// whose name cannot be known until after the exec that creates it.
	// Restoring into the original session's directory would write where the
	// forked session never looks — and when the original is still live, that
	// write lands in a directory it is actively using.
	switch {
	case fork:
		if len(s.Tasks) > 0 {
			fmt.Fprintf(errOut, "forgectl: %d snapshotted task(s) not restored — a fork starts a new session, which reads its own task list\n", len(s.Tasks))
		}
	default:
		if paths, err := resumePaths(); err == nil {
			switch res, err := resume.RestoreFor(paths, s.ID); {
			case err != nil:
				// A failed rescue must not block the resume — the
				// session itself is the thing being recovered.
				fmt.Fprintf(errOut, "forgectl: could not restore tasks: %v\n", err)
			case res.Written > 0:
				fmt.Fprintf(errOut, "forgectl: restored %d task(s)\n", res.Written)
			}
		}
	}

	if err := os.Chdir(s.Cwd); err != nil {
		return WithExitCode(fmt.Errorf("enter %s: %w", s.Cwd, err), 1)
	}

	lc, _ := resolveLaunchConfig(cfg)
	// Resolve is a pure function of the config and an arbitrary cwd, so
	// resuming into another repo gets that repo's profile for free — no
	// per-project config loading.
	profile := launch.Resolve(lc, s.Cwd)
	claudePath, err := launch.ClaudePath(lc.Defaults)
	if err != nil {
		return err
	}

	args := launch.ResumeArgs(profile, profile.Model, s.ID, fork)
	env := launch.MergeEnv(os.Environ(), launch.MergeMaps(bench.TelemetryEnv(cfg), profile.Env))
	fmt.Fprintf(errOut, "forgectl: resuming %s in %s\n", sanitizeTerm(displayName(s)), sanitizeTerm(s.Cwd))
	slog.Debug("Preparing to exec claude for a resume.", "session", s.ID, "cwd", s.Cwd, "fork", fork)
	return launch.Exec(claudePath, args, env)
}

// newResumeLsCmd builds `forgectl resume ls` — the inspection surface.
func newResumeLsCmd() *cobra.Command {
	var asJSON bool
	var limit int

	cmd := &cobra.Command{
		Use:   "ls [filter]",
		Short: "List recent Claude Code sessions without resuming one",
		Long: `ls prints the same rows the picker shows, without acting on them: name,
repo, branch, last activity, liveness, and how many tasks are held for rescue.

--json emits the full records for scripting.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			filter := ""
			if len(args) == 1 {
				filter = args[0]
			}
			sessions, err := scanFor(filter, limit)
			if err != nil {
				return err
			}
			return printSessions(cmd.OutOrStdout(), cmd.ErrOrStderr(), sessions, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON records instead of a table")
	cmd.Flags().IntVar(&limit, "limit", resume.DefaultLimit, "how many recent sessions to list")
	return cmd
}

// sessionDTO is the stable --json shape for `resume ls`.
type sessionDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	NameSource string `json:"name_source,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Cwd        string `json:"cwd"`
	LastActive string `json:"last_active"`
	LastPrompt string `json:"last_prompt,omitempty"`
	Version    string `json:"version,omitempty"`
	Live       bool   `json:"live"`
	Pid        int    `json:"pid,omitempty"`
	Tasks      int    `json:"tasks"`
}

// printSessions renders `resume ls`.
//
// Both paths strip control bytes from every disk-sourced field, and the JSON
// path must do it explicitly: encoding/json escapes only 0x00–0x1F, so DEL and
// the C1 range (0x80–0x9F, including 0x9B = single-byte CSI) would otherwise
// reach a terminal raw through --json. A session name is whatever was typed at
// /rename, and an ai-title is model-generated — neither is trusted markup.
func printSessions(out, errOut io.Writer, sessions []resume.Session, asJSON bool) error {
	if asJSON {
		dto := make([]sessionDTO, 0, len(sessions))
		for _, s := range sessions {
			dto = append(dto, sessionDTO{
				ID: sanitizeTerm(s.ID), Name: sanitizeTerm(s.Name), NameSource: sanitizeTerm(s.NameSource),
				Repo: sanitizeTerm(s.Repo), Branch: sanitizeTerm(s.Branch), Cwd: sanitizeTerm(s.Cwd),
				LastActive: s.LastActive.UTC().Format(time.RFC3339), LastPrompt: sanitizeTerm(s.LastPrompt),
				Version: sanitizeTerm(s.Version), Live: s.Live, Pid: s.Pid, Tasks: len(s.Tasks),
			})
		}
		return writeJSON(out, dto)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(out, "no recent sessions")
		return nil
	}
	for _, s := range sessions {
		fmt.Fprintln(out, sessionRow(s))
		fmt.Fprintf(out, "\t%s\n", sanitizeTerm(s.Cwd))
		if s.LastPrompt != "" {
			fmt.Fprintf(out, "\t%s\n", sanitizeTerm(s.LastPrompt))
		}
	}
	fmt.Fprintf(errOut, "%d session(s)\n", len(sessions))
	return nil
}

// newResumeSnapshotCmd builds `forgectl resume snapshot` — the capture verb.
func newResumeSnapshotCmd() *cobra.Command {
	var quiet bool

	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Capture what a live session's exit would destroy",
		Long: `snapshot records, for every currently-running Claude Code session, the
things that do not survive its exit: the /rename name (which lives only in the
live-process registry) and the task bodies (which Claude Code deletes). It also
records which task directory belongs to which session — an association nothing
on disk keeps.

It is built to run from a Stop hook at every turn end, so it is cheap,
idempotent, and ALWAYS EXITS 0. A capture failure is reported on stderr and
never becomes a failed turn.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runResumeSnapshot(cmd, quiet)
			return nil
		},
	}
	cmd.Flags().BoolVar(&quiet, "quiet", false, "write nothing to stdout (for hook use)")
	return cmd
}

// runResumeSnapshot has no error return by design: it is wired to a hook, and
// nothing it can encounter is worth breaking a turn over.
func runResumeSnapshot(cmd *cobra.Command, quiet bool) {
	errOut := cmd.ErrOrStderr()
	paths, err := resumePaths()
	if err != nil {
		fmt.Fprintf(errOut, "forgectl: snapshot skipped: %v\n", err)
		return
	}
	res := resume.Snapshot(paths, time.Now())
	for _, e := range res.Errs {
		fmt.Fprintf(errOut, "forgectl: snapshot: %v\n", e)
	}
	slog.Debug("Successfully completed resume snapshot.",
		"sessions", res.Sessions, "tasks", res.Tasks, "learned", res.Learned, "errors", len(res.Errs))
	if quiet {
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "snapshotted %d live session(s), %d task(s) held\n", res.Sessions, res.Tasks)
}

// displayName is the session's best label, falling back to the id.
func displayName(s resume.Session) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// cell renders one fixed-width picker column: clipped if too long, padded if
// too short, measured in runes throughout.
//
// fmt's %-28s cannot do this job here. Its width is in BYTES, and truncate's
// ellipsis is three of them — so every clipped name overran its column and
// knocked the rest of the row out of alignment, on exactly the rows a picker
// most needs to stay readable. Session names and branches are non-ASCII often
// enough that this is the normal case, not an edge one.
func cell(s string, width int) string {
	s = truncate(s, width)
	if pad := width - len([]rune(s)); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// truncate clips a string to width runes, marking the clip. A non-positive
// width yields the empty string — unreachable from today's call sites, but
// width-1 would otherwise slice to [:-1] and panic.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len([]rune(s)) <= width {
		return s
	}
	return string([]rune(s)[:width-1]) + "…"
}

// relativeTime renders a timestamp the way the picker needs it: how long ago,
// not when. "3h ago" answers "is this the one I was just in"; an RFC3339 stamp
// makes you do the arithmetic.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.UTC().Format("2006-01-02")
	}
}
