package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/ansi"
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
	var fork, dryRun bool
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
  forgectl resume --dry-run f  resolve and print, exec nothing
  forgectl resume ls --json    list without acting
  forgectl resume snapshot     capture what a session's exit would destroy

THIS COMMAND REPLACES THE PROCESS. On success it execs claude in place (via
syscall.Exec, the same path forgectl launch uses) and never returns — the
resumed session is interactive, so there is no -p/--print form. From a script
or a tool call, use ` + "`resume --dry-run`" + ` (prints the resolved cwd and argv) or
` + "`resume ls --json`" + `; a bare ` + "`resume`" + ` will exec an interactive claude onto
whatever stdout it was given.

Related: ` + "`forgectl launch`" + ` starts or resumes a session in the CURRENT
directory and prompts for the posture. ` + "`resume`" + ` is the cross-repo one — it
finds a session anywhere on the machine and moves you to it.

Session identity is read from Claude Code's own durable files — the prompt
history and per-project transcripts hold the id, cwd, branch, and title
indefinitely. Only two things need capturing, and ` + "`resume snapshot`" + ` is what
captures them: the /rename name, which lives only in the live-process registry,
and the task bodies, which Claude Code deletes when a session ends. Wire it to a
Stop hook — MERGE this entry into the "hooks" object of
~/.claude/settings.json (user scope, so it covers every repo). Do not replace
that file; it holds unrelated settings:

  "Stop": [{"hooks":[
    {"type":"command","command":"forgectl resume snapshot --quiet"}]}]

Without a prior snapshot there is nothing to restore, so an unwired hook fails
silently at exactly the wrong moment. ` + "`forgectl doctor`" + ` warns when live
sessions exist and the store is empty.

A session whose process is still running cannot be CONTINUED — two processes on
one transcript corrupts it — so resume refuses and names the live pid. --fork
gets you in anyway: it branches a new session off the transcript, which only
reads it. Note that --fork applies to DEAD sessions too and always forks: it
starts a new session rather than continuing the old one, and a new session
reads its own (empty) task list, so snapshotted tasks are reported rather than
restored. Pass it in response to the live-session error, not defensively.

Exit codes: 0 resumed; 1 no session matched, or the pick was cancelled; 2 the
target is still running (recoverable — use --fork). Note that ` + "`resume ls`" + `
exits 0 with an empty list when nothing matches, so ` + "`resume ls X && resume X`" + `
is NOT a valid guard; test the ls output instead.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			filter := ""
			if len(args) == 1 {
				filter = args[0]
			}
			return runResume(cmd, deps.Cfg, filter, limit, fork, dryRun)
		},
	}
	cmd.Flags().BoolVar(&fork, "fork", false, "branch a new session off the transcript instead of continuing it")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the resolved session, cwd, and claude argv without resuming")
	cmd.Flags().IntVar(&limit, "limit", resume.DefaultLimit, "how many recent sessions to consider (0 or negative means the default)")

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
func runResume(cmd *cobra.Command, cfg config.Config, filter string, limit int, fork, dryRun bool) error {
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
	} else {
		// A single filter hit execs without the picker, so nothing would
		// otherwise show WHAT is about to be resumed. That matters more
		// than convenience: resuming means chdir'ing into a directory read
		// off disk and exec'ing claude there under a posture that defaults
		// to --allow-dangerously-skip-permissions, at which point claude
		// loads that directory's own .claude/settings.json — hooks
		// included. The operator should see the target first.
		fmt.Fprintf(cmd.ErrOrStderr(), "forgectl: one match — %s\n", sessionRow(picked))
	}
	return resumeSession(cmd, cfg, picked, fork, dryRun)
}

// pickSession runs the single-select. Options are keyed by session id so a
// selection round-trips unambiguously (the same reason pickPRs keys on a ref).
func pickSession(sessions []resume.Session) (resume.Session, error) {
	w := layoutFor(sessions, terminalWidth())
	opts := make([]huh.Option[string], len(sessions))
	for i, s := range sessions {
		label := sessionRowWidth(s, w)
		// A running session cannot be continued, only forked — dimming says
		// so before the selection does, the same way `pr pick` dims a PR it
		// will skip.
		if s.Live {
			label = prDimStyle.Render(label)
		}
		opts[i] = huh.NewOption(label, s.ID)
	}

	// huh v1.0.0 binds Quit to ctrl+c ALONE (keymap.go:109), so Esc is
	// unbound and does nothing — the picker reads as stuck to anyone who
	// reaches for the conventional cancel key. Bind it.
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))

	var chosen string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Recent sessions — enter to resume, esc to cancel").
				Options(opts...).
				Value(&chosen),
		),
	).WithKeyMap(km).Run()
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

// rowLayout is one render's column widths, in runes.
type rowLayout struct{ name, repo, branch int }

// Column bounds. The floors keep a column identifiable when space is tight;
// the caps stop one long value from starving the others on a wide terminal.
const (
	minNameCol, maxNameCol     = 16, 52
	minRepoCol, maxRepoCol     = 8, 28
	minBranchCol, maxBranchCol = 6, 24
	// tailWidth reserves the trailing columns the layout does not pad: the
	// relative time at its widest (a bare "2026-07-30" once a session is over
	// a month old, 10) plus the longest annotation ("  (running, pid 123456)",
	// 23), with a little headroom. Undercounting here does not truncate — it
	// wraps the row, which is worse.
	tailWidth = 36
)

// defaultLayout is what a non-terminal render uses: stable widths, so piped
// output does not reflow depending on who is watching.
var defaultLayout = rowLayout{name: 28, repo: 22, branch: 18}

// writerWidth reports the usable width of the writer actually being rendered
// to, or 0 when it is not a terminal.
//
// Asking about os.Stdout would be the wrong question: printSessions renders to
// cmd.OutOrStdout(), which a caller can redirect to a pipe, a file, or a test
// buffer while the process's own stdout is still a tty. That combination would
// have laid out piped output at terminal widths — reflow depending on who was
// watching, which is exactly what the non-terminal fallback exists to prevent.
func writerWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return 0
	}
	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

// terminalWidth reports the width of the process's own stdout — what the
// picker renders to, since huh drives the terminal directly rather than a
// writer we hand it.
func terminalWidth() int { return writerWidth(os.Stdout) }

// layoutFor sizes the columns to the CONTENT and then to the TERMINAL.
//
// Fixed widths were wrong in both directions at once: they clipped names that
// would have fit on a wide terminal while padding the branch column to 18, so
// a list where almost every branch is "main" spent a quarter of the row on
// whitespace. Repo and branch are therefore measured against what is actually
// in them, and the name — the column that identifies the row — takes whatever
// is left over.
func layoutFor(sessions []resume.Session, width int) rowLayout {
	if width <= 0 {
		return defaultLayout
	}
	l := rowLayout{name: minNameCol, repo: minRepoCol, branch: minBranchCol}
	for _, s := range sessions {
		l.repo = max(l.repo, displayWidth(s.Repo))
		l.branch = max(l.branch, displayWidth(s.Branch))
	}
	l.repo = min(l.repo, maxRepoCol)
	l.branch = min(l.branch, maxBranchCol)

	// Budget for the three padded columns: the terminal less the reserved
	// tail and the three single-space separators between four columns.
	avail := width - tailWidth - 3
	if avail <= minNameCol+minRepoCol+minBranchCol {
		// Genuinely too narrow for all three floors. Nothing fits; use the
		// floors and let the terminal wrap rather than rendering columns so
		// thin they identify nothing.
		return rowLayout{name: minNameCol, repo: minRepoCol, branch: minBranchCol}
	}

	l.name = min(avail-l.repo-l.branch, maxNameCol)
	if l.name < minNameCol {
		// The content-sized repo and branch have squeezed the name below its
		// floor. Take the space back from them — widest-capped first — rather
		// than letting a long repo name push the row past the terminal, which
		// wraps instead of clipping and is the worse failure.
		deficit := minNameCol - l.name
		give := min(deficit, l.branch-minBranchCol)
		l.branch -= give
		deficit -= give
		l.repo -= min(deficit, l.repo-minRepoCol)
		l.name = minNameCol
	}
	return l
}

// sessionRow renders one picker row at the default layout — the shape tests
// and non-terminal callers get.
func sessionRow(s resume.Session) string {
	return sessionRowWidth(s, defaultLayout)
}

// sessionRowWidth renders one row at an explicit layout. Every field is
// disk-sourced, so every field goes through sanitizeTerm.
func sessionRowWidth(s resume.Session, l rowLayout) string {
	name := s.Name
	if name == "" {
		name = s.ID
	}
	row := fmt.Sprintf("%s %s %s %s",
		cell(sanitizeTerm(name), l.name),
		cell(sanitizeTerm(s.Repo), l.repo),
		cell(sanitizeTerm(s.Branch), l.branch),
		relativeTime(s.LastActive))
	switch {
	case s.Live:
		row += fmt.Sprintf("  (running, pid %d)", s.Pid)
	case len(s.Tasks) > 0:
		row += fmt.Sprintf("  (%d tasks)", len(s.Tasks))
	}
	return row
}

// displayWidth measures a string in TERMINAL CELLS, not runes.
//
// The distinction is not pedantic here: a CJK ideograph or an emoji occupies
// two cells, a combining mark occupies none, and a session name is arbitrary
// text someone typed at /rename or a model generated as an ai-title. Measuring
// runes made a name with wide characters overflow its column and shove every
// column after it out of alignment — the exact defect the fixed widths were
// replaced to fix, reintroduced one layer down.
func displayWidth(s string) int { return ansi.StringWidth(s) }

// resumeSession restores the session's tasks, moves into its directory, and
// replaces this process with claude.
func resumeSession(cmd *cobra.Command, cfg config.Config, s resume.Session, fork, dryRun bool) error {
	errOut := cmd.ErrOrStderr()

	// The one failure mode this feature introduces. Two claude processes
	// CONTINUING one transcript is corruption, not a resume — so it is
	// refused rather than warned about, and the pid is named so the session
	// is findable. --fork is the documented way past it, and it is a real
	// escape rather than a suggestion: a fork only READS the transcript and
	// writes to a new session of its own, so it does not contend.
	//
	// Exit 2, not 1, because this is the one refusal a caller can act on: it
	// is recoverable with --fork, where "no session matched" is not. The
	// binary already draws that 1-vs-2 line elsewhere (`env check`).
	//
	// Held as a value rather than returned here so --dry-run can still
	// DESCRIBE a run it would block. A dry-run that refuses to tell you what
	// it would have done is not an inspection surface, and inspecting a
	// running session is the common case for one.
	var blocked error
	if s.Live && !fork {
		blocked = WithExitCode(fmt.Errorf(
			"session %s (%s) is still running as pid %d — continuing it a second time would corrupt the transcript; switch to that terminal, or pass --fork to branch a new session off it",
			sanitizeTerm(displayName(s)), sanitizeTerm(s.ID), s.Pid), 2)
	}
	// Refuse immediately unless this is a dry-run. Deferring it would make the
	// refusal depend on claude being installed and the cwd still existing,
	// neither of which is relevant to "that session is already running".
	if blocked != nil && !dryRun {
		return blocked
	}
	if s.Cwd == "" {
		return WithExitCode(fmt.Errorf("session %s has no recorded working directory to resume into", sanitizeTerm(s.ID)), 1)
	}

	// Confirm the target is a real directory BEFORE anything writes. Task
	// restore used to run first, so a resume that then failed on a deleted
	// repo had already written files and announced doing so — a side effect
	// on an otherwise-failed command.
	//
	// Sanitize BOTH the path and the error text on the way out. This is the
	// most reachable escape sink in the file — os.Chdir fails precisely when
	// the path is malformed — and %w would have re-printed the raw path a
	// second time inside the wrapped *os.PathError, so sanitizing only s.Cwd
	// would have left the hole open.
	if fi, err := os.Stat(s.Cwd); err != nil || !fi.IsDir() {
		return WithExitCode(fmt.Errorf("session %s records a working directory that is not there: %s",
			sanitizeTerm(s.ID), sanitizeTerm(s.Cwd)), 1)
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
	args := launch.ResumeArgs(profile, s.ID, fork)

	// --dry-run is the headless escape. Resuming exec-replaces this process
	// with an INTERACTIVE claude (ResumeArgs injects --ide, which rules out a
	// -p/--print form), so without this there is no way to ask "what would
	// this do" from a script. Matches `pr <ref> --dry-run` and
	// `workflow run --dry-run`.
	if dryRun {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "session %s\n", sanitizeTerm(s.ID))
		fmt.Fprintf(out, "name    %s\n", sanitizeTerm(displayName(s)))
		fmt.Fprintf(out, "cwd     %s\n", sanitizeTerm(s.Cwd))
		fmt.Fprintf(out, "exec    %s %s\n", sanitizeTerm(claudePath), sanitizeTerm(strings.Join(args, " ")))
		fmt.Fprintf(out, "tasks   %d held%s\n", len(s.Tasks), forkTaskNote(fork))
		if blocked != nil {
			fmt.Fprintf(out, "blocked live — pid %d; add --fork to branch instead\n", s.Pid)
		}
		// Exits with the code the real run would, so a script can trust the
		// dry-run's verdict and not just its text.
		return blocked
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
	//
	// The fork notice prints UNCONDITIONALLY, including at zero tasks. It
	// used to be suppressed when there was nothing to restore — which is the
	// state of every machine without the Stop hook wired, so the warning went
	// quiet exactly where the operator was least protected.
	switch {
	case fork:
		fmt.Fprintf(errOut, "forgectl: forking — %d snapshotted task(s) not restored; a fork starts a new session, which reads its own task list\n", len(s.Tasks))
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
		return WithExitCode(fmt.Errorf("enter %s: %s", sanitizeTerm(s.Cwd), sanitizeTerm(err.Error())), 1)
	}

	env := launch.MergeEnv(os.Environ(), launch.MergeMaps(bench.TelemetryEnv(cfg), profile.Env))
	fmt.Fprintf(errOut, "forgectl: resuming %s in %s\n", sanitizeTerm(displayName(s)), sanitizeTerm(s.Cwd))
	slog.Debug("Preparing to exec claude for a resume.", "session", s.ID, "cwd", s.Cwd, "fork", fork)
	return launch.Exec(claudePath, args, env)
}

// forkTaskNote annotates the dry-run task line so a fork's task behavior is
// visible without running it.
func forkTaskNote(fork bool) string {
	if fork {
		return " (not restored — forking starts a new session)"
	}
	return ""
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
Unlike the parent command it never execs anything.

--json emits one record per session on stdout; the "N session(s)" count goes to
stderr, so stdout stays pipeable. Fields:

  id           session uuid; always present
  name         display name; ABSENT when unknown
  name_source  where name came from: rename | store | ai-title | lane
  repo         basename of cwd; absent when no cwd is recorded
  branch       git branch from the transcript; absent when unknown
  cwd          always present
  last_active  RFC3339 UTC
  last_prompt  FIRST LINE of the last prompt, truncated for display — absent
               when unknown, and never the full prompt text
  version      Claude Code version that wrote the session; absent when unknown
  live         bool; true when the session's process is still running
  pid          absent unless live
  tasks        COUNT of snapshotted task bodies held, not the bodies

Fields marked absent are omitted from the object entirely rather than emitted
as null. Use live/pid to pre-check before calling the parent command, which
refuses a running session with exit 2.

EXIT CODES DIFFER FROM THE PARENT: ls exits 0 even when nothing matches (an
empty list is not an error), emitting [] under --json. Do not use it as a
guard for a following resume.`,
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
	// Same layout rule as the picker, but measured against the writer we are
	// actually rendering to: sized to the terminal when there is one, fixed
	// when the output is piped so a consumer sees stable columns.
	l := layoutFor(sessions, writerWidth(out))
	for _, s := range sessions {
		fmt.Fprintln(out, sessionRowWidth(s, l))
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
never becomes a failed turn.

The store self-prunes, at most once a day. What retires a record is TRANSCRIPT
EXISTENCE, not a fixed age: a snapshot is worth keeping exactly as long as
'claude --resume' can still open that session, and how long that is depends on
Claude Code's own transcript retention, which is operator-configurable. A record
whose transcript survives is KEPT, however old it is — age never overrides a
session you can still open. A 180-day ceiling applies only to records whose
transcript is already gone, and exists to retire those even on a machine where
the transcript lookup itself is failing.

Pruning refuses to run away: a pass that would retire most of the store deletes
nothing and says so on stderr, because that is what a broken transcript lookup
looks like. Set FORGECTL_RESUME_NO_PRUNE to any non-empty value to disable
pruning entirely.`,
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
		"sessions", res.Sessions, "tasks", res.Tasks, "learned", res.Learned,
		"pruned", res.Pruned, "swept", res.Swept, "errors", len(res.Errs))
	if quiet {
		return
	}
	line := fmt.Sprintf("snapshotted %d live session(s), %d task(s) held", res.Sessions, res.Tasks)
	if res.Pruned > 0 {
		line += fmt.Sprintf(", %d stale record(s) pruned", res.Pruned)
	}
	if res.Swept > 0 {
		line += fmt.Sprintf(", %d orphan file(s) deleted", res.Swept)
	}
	fmt.Fprintln(cmd.OutOrStdout(), line)
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
	if pad := width - displayWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// truncate clips a string to width terminal CELLS, marking the clip with an
// ellipsis. A non-positive width yields the empty string.
//
// ansi.Truncate does the cell accounting, which a rune slice cannot: it will
// not split a double-width character across the boundary, and it does not
// spend budget on zero-width combining marks.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
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
