// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// deskModule declares `desk`: the operator queue for scripts Claude stages but
// will not run itself, which the operator approves and runs. The threat model
// is docs/adr/0012-desk-threat-model.md; the protocol is docs/commands/desk.md.
var deskModule = module.Manifest{
	Name:      "desk",
	Tier:      module.TierExtension,
	ConfigKey: "desk",
	New:       newDeskCmd,
}

// Exit codes the desk verbs share beyond 0 and 1.
const (
	// deskExitUsage: a flag or argument was wrong, or a precondition (a
	// terminal, a herdr pane) is missing. Nothing was changed.
	deskExitUsage = 2
	// deskExitTempFail: `desk watch` reached its --deadline before the run
	// ended (EX_TEMPFAIL); the printed resume= line picks up where it stopped.
	deskExitTempFail = 75
	// deskExitInterrupted: `desk watch` was interrupted by a signal.
	deskExitInterrupted = 130
	// deskExitBrokenPipe: `desk watch` could not write to stdout (its reader
	// went away), 128+SIGPIPE as a shell reports a process killed by it.
	deskExitBrokenPipe = 141
	// deskQuoteMax caps an argument quoted back in an error.
	deskQuoteMax = 120
)

// Injectable seams, so command tests need no home directory, terminal, or
// real clock.
var (
	deskGetenv        = os.Getenv
	deskUserHome      = os.UserHomeDir
	deskNow           = time.Now
	deskWatchInterval = 500 * time.Millisecond
)

// deskUsage marks err as a usage or precondition failure (exit 2).
func deskUsage(format string, args ...any) error {
	return WithExitCode(fmt.Errorf(format, args...), deskExitUsage)
}

// errDeskUnsupported is every desk verb's refusal off Unix: the desk is built
// on openat, O_NOFOLLOW, and process sessions and groups.
func errDeskUnsupported() error {
	return WithExitCode(fmt.Errorf("forgectl desk is not supported on this OS (%s); it needs a Unix system", runtime.GOOS), deskExitUsage)
}

// resolveDeskDir is --dir when given (made absolute), else desk.ResolveDir.
func resolveDeskDir(flag string) (string, error) {
	if flag != "" {
		abs, err := filepath.Abs(flag)
		if err != nil {
			return "", fmt.Errorf("desk: --dir: %w", err)
		}
		return abs, nil
	}
	// A missing home only matters when no env var names the dir, and then
	// ResolveDir refuses; report both causes together.
	home, homeErr := deskUserHome()
	dir, err := desk.ResolveDir(deskGetenv, home)
	if err != nil {
		return "", errors.Join(err, homeErr)
	}
	return dir, nil
}

// checkDeskName refuses an argument that is not an item name, before the
// desk is opened.
func checkDeskName(name string) error {
	if !desk.ValidName(name) {
		return deskUsage("desk: %q is not an item name (NN-name, as `forgectl desk add` prints it)", termsafe.SafeLineMax(name, deskQuoteMax))
	}
	return nil
}

func newDeskCmd(deps module.Deps) *cobra.Command {
	var dir string
	var frame bool
	cmd := &cobra.Command{
		Use:   "desk",
		Short: "Operator queue: scripts Claude stages for you to approve and run",
		Long: `desk is a queue of scripts Claude will not run itself. Claude stages an item
with ` + "`desk add`" + `; you read it on the dashboard, check its short sha256
against the one Claude reported, and press y to run it, or s to skip it. Each
item's sha256 is fixed when it is queued, and an item whose bytes change
afterwards is skipped as "changed" instead of run.

  forgectl desk                  the dashboard (needs a terminal)
  forgectl desk --frame          one frame to stdout, sized by $COLUMNS/$LINES
  forgectl desk add FILE ...     queue a script (.sh) or batch (.manifest)
  forgectl desk plan NAME|FILE   check a batch manifest: waves, warnings, sha256
  forgectl desk status [NAME]    the queue, or one item in detail
  forgectl desk watch NAME       stream a run's events; exit with its outcome
  forgectl desk runs             every run and how far it got
  forgectl desk show NAME        one run as a flow; --events, --at N to replay
  forgectl desk skip NAME        skip a waiting item, or clear a lost run
  forgectl desk layout           herdr split: Claude left, the desk right
  forgectl desk prune            delete done/ and skipped/ entries older than N days

The desk directory is --dir, else $DESK_DIR, else $CLAUDE_DESK_DIR, else
$XDG_STATE_HOME/forgectl/desk (~/.local/state/forgectl/desk). Only its
pending/, running/, done/ and skipped/ subdirectories are touched.

Dashboard keys: y run, s skip (asks first), u undo a skip, v view the script,
l the latest log, r the run view (flow, events, replay), a run everything on screen (asks first, listing each full
sha256), j/k move, q quit.

The desk defends against accidents (an item edited after it was queued, an item
run twice, hostile text in a header); it does not defend against another
process of yours pressing keys or editing files. The hash covers the item's own
bytes, not anything the script sources, calls or downloads.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeskDashboard(cmd, deps, dir, frame)
		},
	}
	cmd.PersistentFlags().StringVar(&dir, "dir", "", "desk directory (default: $DESK_DIR, $CLAUDE_DESK_DIR, then the XDG state dir)")
	cmd.Flags().BoolVar(&frame, "frame", false, "print one frame to stdout and exit; size from $COLUMNS and $LINES (default 80x40)")
	cmd.AddCommand(
		newDeskAddCmd(&dir, deps),
		newDeskPlanCmd(&dir),
		newDeskStatusCmd(&dir),
		newDeskWatchCmd(&dir),
		newDeskRunsCmd(&dir),
		newDeskShowCmd(&dir),
		newDeskSkipCmd(&dir, deps),
		newDeskLayoutCmd(deps, &dir),
		newDeskPruneCmd(&dir),
		newDeskSuperviseCmd(&dir),
	)
	return cmd
}

// deskAddOpts are `desk add`'s flags.
type deskAddOpts struct {
	what, why, name string
	tty, asJSON     bool
}

func newDeskAddCmd(dir *string, deps module.Deps) *cobra.Command {
	var o deskAddOpts
	cmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Queue a script (.sh) or batch manifest (.manifest) for the operator",
		Long: `add copies FILE into pending/ as the next NN-<name>, with "# WHAT:" and
"# WHY:" header lines inserted (after a shebang), and fixes its sha256. It
prints name=, kind= and sha256= lines. Report the name and the sha256 to the
operator: the dashboard's focus panel shows the selected item's first 12 hex
characters ("17 merge-1201 · sha256 3f1a9c0d2b7e · unchanged since queued
12m ago") for the operator to match before pressing y; a, which runs several
items, asks first and lists each with its full hash.

--what and --why are one line of plain text each: a control character
(newline, CR, ESC, C1) or a bidi control exits 2. The file must not already
carry a "# WHAT:" or "# WHY:" line.

FILE must be a regular file (a symlink to one is followed); a FIFO or device is
refused, never read. FILE "-" reads the item from stdin; --name then gives its
file name (deploy.sh, nightly.manifest), whose extension picks the kind. stdin
must not be a terminal.

When an item is queued, add tells the operator: a herdr notification and the
queuing pane's needs-you state (inside herdr) and a macOS notification. A
failed signal is a warning: line and never fails the add; [desk] notify_herdr
and notify_macos in config.toml turn each off.

A .manifest is checked like ` + "`desk plan`" + ` before it is queued: a manifest that cannot
run is refused, and its warnings are printed as warning: lines on stderr.

--tty marks a script that needs the terminal (a password prompt, sudo); it runs
in the dashboard's own pane. A batch cannot be a TTY item.

Exit codes: 0 queued; 1 refused (unreadable or non-regular file, bad
manifest, no free number); 2 usage (missing, empty or unsafe --what/--why, a
bad --name).`,
		Example: `  forgectl desk add ./merge-1201.sh --what "Merge PR 1201 once green" --why "You own merges"
  printf 'echo hi\n' | forgectl desk add - --name hi.sh --what "Say hi" --why "A test"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeskAdd(cmd, deps, *dir, args[0], o)
		},
	}
	cmd.Flags().StringVar(&o.what, "what", "", "what the script does, one line (required)")
	cmd.Flags().StringVar(&o.why, "why", "", "why it needs the operator, one line (required)")
	cmd.Flags().BoolVar(&o.tty, "tty", false, "the script needs a terminal; it runs in the dashboard's pane")
	cmd.Flags().StringVar(&o.name, "name", "", "file name for an item read from stdin (FILE -), e.g. deploy.sh")
	cmd.Flags().BoolVar(&o.asJSON, "json", false, "print the queued item as one JSON object")
	return cmd
}

// checkFlagText refuses text the desk writes into a script or its meta when
// it carries a control character (a newline, CR, ESC, C1) or a bidi control:
// the desk shows it escaped, but cat or an editor would not.
func checkFlagText(flag, v string) error {
	for _, r := range v {
		if termsafe.IsUnsafeTerminalRune(r) {
			return deskUsage("desk: %s must be one line of plain text (no control or bidi characters)", flag)
		}
	}
	return nil
}

// checkAddFlags is add's usage check, before anything is read.
func checkAddFlags(file string, o deskAddOpts) error {
	if strings.TrimSpace(o.what) == "" {
		return deskUsage("desk add: --what is required and must not be empty (one line: what the script does)")
	}
	if strings.TrimSpace(o.why) == "" {
		return deskUsage("desk add: --why is required and must not be empty (one line: why it needs the operator)")
	}
	if err := checkFlagText("--what", o.what); err != nil {
		return err
	}
	if err := checkFlagText("--why", o.why); err != nil {
		return err
	}
	switch {
	case file == "-" && o.name == "":
		return deskUsage("desk add: reading stdin (FILE -) needs --name, e.g. --name deploy.sh")
	case file != "-" && o.name != "":
		return deskUsage("desk add: --name is only for an item read from stdin (FILE -)")
	case o.name != "" && (filepath.Base(o.name) != o.name || strings.HasPrefix(o.name, ".")):
		return deskUsage("desk add: --name must be a plain file name like deploy.sh, not a path")
	}
	return nil
}

func newDeskPlanCmd(dir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "plan <name|file>",
		Short: "Check a batch manifest: its waves, warnings, and sha256",
		Long: `plan parses a batch manifest and prints how it would run, without running
it: the sha256, the step count, the waves in order (a,b -> c), and one warning:
line per problem that does not stop it (a step that reads OUT_<x>_<k> from a
step that is not its ancestor).

The argument is a file when it ends in .manifest or contains a "/", and an item
name (NN-name) otherwise; an item is read from the desk (pending first, then
running, done and skipped).

Manifest grammar, one step per line ("#" comments and blank lines ignored):

  <step> [after=a,b] [timeout=S] [private] -- <command>

Exit codes: 0 the manifest can run (warnings or not); 1 it cannot, or the item
is not a manifest or was not found; 2 usage.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeskPlan(cmd, *dir, args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the plan as one JSON object")
	return cmd
}

func newDeskStatusCmd(dir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [name]",
		Short: "Show the queue, or one item and its batch summary",
		Long: `status prints the queue: waiting, running, done (the 20 most recent; --json
lists all) and skipped items, one line each, with the state, age, a short
sha256, and the exit code. A waiting item older than 24h is flagged stale; a
running item whose supervisor is gone with no RUN-END is "lost" (clear it with
` + "`desk skip`" + `).

With NAME it prints that item in detail as key=value lines, including the full
sha256 and, for a batch, its steps (the run's summary.json once it has
finished).

Like the dashboard, status fixes the hash of a hand-dropped item the first time
it sees it, and moves a pending item whose bytes changed to skipped/.

Exit codes: 0 shown; 1 no such item, or the desk could not be read; 2 usage.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runDeskStatus(cmd, *dir, name, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the queue (or the item) as one JSON object")
	return cmd
}

func newDeskWatchCmd(dir *string) *cobra.Command {
	var deadline, skip int
	cmd := &cobra.Command{
		Use:   "watch <name>",
		Short: "Stream a run's event lines and exit with its outcome",
		Long: `watch prints NAME's event lines (RUN-START, STEP-START, STEP-END, STEP-SKIP,
STEP-WARN, RUN-END) as they arrive, one per line, and exits when the run does.
It waits while the item is still pending, and reads no input, so it is safe
under a monitor with no terminal.

Exit codes:
   0  RUN-END with rc=0
   1  RUN-END with another rc; RUN-LOST (the supervisor is gone with no
      RUN-END); the item was skipped; or no such item
   2  usage
  75  --deadline passed first; the last line is
      resume=forgectl desk watch NAME --skip N ...
      which picks up after the N lines already printed
 130  interrupted
 141  stdout closed (a write failed); the watch stops at once and the
      resume= command is in the error on stderr

--skip N leaves out the first N event lines, the resume point a previous
watch printed.`,
		Example: `  forgectl desk watch 17-merge-1201 --deadline 540`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if deadline < 0 || skip < 0 {
				return deskUsage("desk watch: --deadline and --skip must not be negative")
			}
			return runDeskWatch(cmd, *dir, args[0], deadline, skip)
		},
	}
	cmd.Flags().IntVar(&deadline, "deadline", 0, "stop after S seconds with exit 75 and a resume= line (0: wait for the run)")
	cmd.Flags().IntVar(&skip, "skip", 0, "leave out the first N event lines (a previous watch's resume point)")
	return cmd
}

func newDeskSkipCmd(dir *string, deps module.Deps) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "skip <name> --reason <text>",
		Short: "Skip a waiting item, or clear a lost run out of running/",
		Long: `skip moves a waiting item to skipped/ (the dashboard's u can return it), or a
lost run (its supervisor is gone with no RUN-END) out of running/, which is the
only way a lost run leaves it; a lost run cannot be re-armed. A live run is
refused. The --reason text (one line of plain text, at most 200 characters)
is kept as the item's skip_note, with skipped_by "cli" and the time.

Exit codes: 0 skipped; 1 no such item, or it is running, or another desk took
it first; 2 usage (an empty, long or unsafe --reason).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeskSkip(cmd, deps, *dir, args[0], reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, one line (required)")
	return cmd
}

func newDeskPruneCmd(dir *string) *cobra.Command {
	var days int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete done/ and skipped/ items older than --days",
		Long: `prune deletes the protocol files (.sh, .manifest, .log, .events, .meta.json,
.d/) of items in done/ and skipped/ whose newest file is older than --days. It
never touches pending/ or running/, unknown files, symlinks, or anything at the
desk root. Nothing else ever deletes desk files: prune runs only when asked.

Exit codes: 0 pruned (maybe nothing); 1 a delete failed; 2 usage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if days < 1 {
				return deskUsage("desk prune: --days must be at least 1")
			}
			return runDeskPrune(cmd, *dir, days, asJSON)
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "delete items older than this many days")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the count as one JSON object")
	return cmd
}

// newDeskSuperviseCmd builds `desk _supervise --sha SHA --kind KIND NAME`:
// the detached process that owns one claimed item's run. The desk starts it
// in a session of its own (desk.Launch), so it outlives the desk's pane. It
// runs only an item already in running/ whose bytes hash to --sha, the hash
// the operator approved, which must also be the hash fixed at queue time,
// and that is still the --kind it was approved as. Invoking it by hand grants
// nothing the invoker could not do with bash. Hidden is presentation, not a
// control.
func newDeskSuperviseCmd(dir *string) *cobra.Command {
	var sha, kind string
	cmd := &cobra.Command{
		Use:          "_supervise --sha SHA --kind KIND <name>",
		Short:        "Internal: run one claimed desk item to completion",
		Hidden:       true,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runDeskSupervise(*dir, args[0], sha, kind)
		},
	}
	cmd.Flags().StringVar(&sha, "sha", "", "the approved item's full sha256; the item runs only if its bytes still match")
	cmd.Flags().StringVar(&kind, "kind", "", "the approved item's kind, script or batch; the item runs only as that kind")
	for _, f := range []string{"sha", "kind"} {
		_ = cmd.MarkFlagRequired(f) // fails only for a flag that does not exist, and both are defined above
	}
	return cmd
}
