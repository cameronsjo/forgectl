package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

// installedVersionFn is the seam for resolving the installed harness version,
// so a cli test does not depend on the developer's own claude install.
var installedVersionFn = func(ctx context.Context, deps module.Deps) (string, error) {
	lc, _ := resolveLaunchConfig(deps.LegacyBoundary, deps.Cfg, "")
	// Same lookup `forgectl launch` uses, so FORGECTL_CLAUDE_BIN and
	// [launch.defaults] binary_path are respected.
	claudePath, err := launch.ClaudePath(lc.Defaults)
	if err != nil {
		return "", fmt.Errorf("locate the claude binary: %w", err)
	}
	return resume.InstalledVersion(ctx, claudePath, deps.Runner)
}

// newResumeOutdatedCmd builds `forgectl resume outdated` — the read-only list
// of live sessions running an older harness than the one installed.
func newResumeOutdatedCmd(deps module.Deps) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "outdated",
		Short: "List live Claude Code sessions running an older version than the one installed",
		Long: `outdated lists the running Claude Code sessions whose recorded version is
older than the installed claude. It is read-only: it signals no process, writes
to no pane, and modifies no registry file.

The installed version is the basename of the claude binary's symlink target
(~/.local/bin/claude -> versions/<X>), falling back to ` + "`claude --version`" + `. The
binary is found the way ` + "`forgectl launch`" + ` finds it, so FORGECTL_CLAUDE_BIN and
[launch.defaults] binary_path are respected. Versions compare numerically per
segment (2.1.100 is newer than 2.1.99).

Only sessions whose process is still running are listed; a registry file whose
pid is dead is skipped. A session whose recorded version cannot be parsed is
listed and marked, since nothing proves it current.

--json emits an array on stdout; [] when nothing is outdated. Fields:

  session_id           session uuid
  pid                  process id
  cwd                  working directory
  status               registry status, verbatim (busy | idle | shell | ...)
  busy                 bool; true for every status except "idle", so an
                       unknown status counts as busy
  version              the session's version, verbatim
  installed_version    the installed claude version
  version_unparseable  bool; true when version could not be compared
  pane                 HERDR_PANE_ID of the process; "" when unknown

The pane is read from the process environment (ps eww on macOS,
/proc/<pid>/environ on Linux), same-user only; failing to read it leaves it
empty and never fails the command.

Exit 0 whether or not anything is outdated; non-zero only when the installed
version or the session registry cannot be read.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			paths, err := resumePaths()
			if err != nil {
				return fmt.Errorf("locate session records: %w", err)
			}
			installed, err := installedVersionFn(ctx, deps)
			if err != nil {
				return err
			}
			list, err := resume.Outdated(paths, installed, resume.PaneFor(ctx, deps.Runner))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			return printOutdated(out, list, asJSON, writerWidth(out) > 0)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a JSON array instead of a table")
	return cmd
}

// outdatedDTO is the stable --json shape for `resume outdated`.
type outdatedDTO struct {
	SessionID          string `json:"session_id"`
	Pid                int    `json:"pid"`
	Cwd                string `json:"cwd"`
	Status             string `json:"status"`
	Busy               bool   `json:"busy"`
	Version            string `json:"version"`
	InstalledVersion   string `json:"installed_version"`
	VersionUnparseable bool   `json:"version_unparseable"`
	Pane               string `json:"pane"`
}

// printOutdated renders the list. Every string is registry-derived and
// untrusted (another process wrote it): the JSON path leaves escaping to
// writeJSON's encoder, the table path quotes through safeTerm.
func printOutdated(out io.Writer, list []resume.OutdatedSession, asJSON, tty bool) error {
	if asJSON {
		dto := make([]outdatedDTO, 0, len(list))
		for _, s := range list {
			dto = append(dto, outdatedDTO{
				SessionID: s.SessionID, Pid: s.Pid, Cwd: s.Cwd,
				Status: s.Status, Busy: s.Busy,
				Version: s.Version, InstalledVersion: s.InstalledVersion,
				VersionUnparseable: s.VersionUnparseable, Pane: s.Pane,
			})
		}
		return writeJSON(out, dto)
	}
	if len(list) == 0 {
		// A pipe gets nothing: an empty table is the empty answer.
		if tty {
			_, err := fmt.Fprintln(out, "no outdated sessions")
			return err
		}
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	// tabwriter buffers, so a write error surfaces from Flush.
	_, _ = fmt.Fprintln(tw, "SESSION\tSTATUS\tVERSION\tINSTALLED\tPANE\tCWD")
	for _, s := range list {
		version := safeTerm(s.Version)
		if s.VersionUnparseable {
			version = fmt.Sprintf("%q (unparseable)", version)
		}
		pane := safeTerm(s.Pane)
		if pane == "" {
			pane = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			safeTerm(s.SessionID), safeTerm(s.Status), version,
			safeTerm(s.InstalledVersion), pane, safeTerm(s.Cwd))
	}
	return tw.Flush()
}
