package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxPickCmd connects to (or smart-creates) a session via sesh. With a
// name it connects directly; with no name it prints the candidate list — the
// TUI picker (M5) is the zero-typing no-arg experience.
func newTmuxPickCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "pick [name]",
		Short: "Connect to or smart-create a session (via sesh)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return seshPick(cmd.Context(), client, args[0])
			}
			names, err := client.SeshList(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(names) == 0 {
				fmt.Fprintln(out, "no sesh candidates")
				return nil
			}
			// A sesh candidate is a tmux session name or a directory sesh
			// discovered — neither composed by forgectl, both printable to a
			// terminal only after neutralizing.
			for _, n := range names {
				_, _ = fmt.Fprintln(out, safeText(n))
			}
			return nil
		},
	}
}

// errSeshUnsafeCandidate refuses a sesh candidate that is, or resolves to, a
// name containing '#'.
//
// sesh (v2.31.0 and earlier) builds its own `tmux new-session -c <path>` from
// the candidate and does not escape '#', and tmux format-expands the value of
// -c. A directory named `x#(cmd)`, which any same-uid process can create and
// which surfaces in the picker through sesh's zoxide and directory sources,
// would run cmd in the tmux server the moment it is picked. forgectl escapes
// its own -c values (#839), but it cannot reach into sesh's, so it refuses to
// hand sesh the name at all. This over-refuses an existing tmux session whose
// name contains '#' (sesh would only attach to it); the TUI's sessions view
// still attaches to that session by ID.
//
// A '#' can also reach sesh's tmux argv from a candidate that has none:
//
//   - sesh expands env vars and a leading ~, makes the candidate absolute
//     against the inherited working directory (so "." names a '#' cwd), and
//     its namer resolves symlinks before naming the session, so a symlink to
//     a '#' directory carries the '#' into -s. seshPick closes these by
//     resolving the candidate in sesh's own order (seshResolvedPaths). The
//     check is best-effort: a link can be retargeted between the check and
//     sesh's own resolution.
//   - sesh's git namer names a linked worktree after its main worktree's root,
//     and sesh's zoxide lookup fuzzy-matches a '#'-free query to a '#' path.
//     Neither is visible from here without re-implementing sesh's strategy
//     chain; only the upstream fix closes them (#841).
var errSeshUnsafeCandidate = errors.New("refusing to hand sesh a name containing '#': sesh passes it to tmux unescaped, where tmux would expand #(...) as a command")

// seshPick is the single forgectl-side gate in front of `sesh connect`. Both
// the `tmux pick <name>` command and the TUI picker's hand-off route through
// it.
//
// A refusal on a resolved path echoes only the candidate, never the resolved
// path: resolution runs os.ExpandEnv, and a candidate like `$SOME_TOKEN/x`
// must not turn the error into a print of an environment value.
func seshPick(ctx context.Context, client *tmux.Client, name string) error {
	if strings.Contains(name, "#") {
		return fmt.Errorf("%w: %s", errSeshUnsafeCandidate, termsafe.QuotePath(name))
	}
	paths, err := seshResolvedPaths(name, os.UserHomeDir)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errSeshUnsafeCandidate, termsafe.QuotePath(name), err)
	}
	for _, p := range paths {
		if strings.Contains(p, "#") {
			return fmt.Errorf("%w: %s resolves to a path containing '#'",
				errSeshUnsafeCandidate, termsafe.QuotePath(name))
		}
	}
	return client.Pick(ctx, name)
}

// seshResolvedPaths returns the paths sesh can derive from a candidate, in the
// order sesh v2.31.0 derives them:
//
//  1. os.ExpandEnv, then one leading "~" replaced with the home directory by
//     plain string replacement, so "~user" becomes "<home>user"
//     (home/home.go:35-44, RealHome.ExpandPath);
//  2. filepath.Abs against the working directory, which sesh inherits from
//     forgectl (connector/dir.go:12 via dir/dir.go:27) — the path handed to
//     `new-session -c`;
//  3. filepath.EvalSymlinks, which the namer applies before naming the
//     session (namer/namer.go:36) — the source of `-s`.
//
// The Abs path is returned even when EvalSymlinks fails, so a relative
// candidate such as "." or "sub/.." is checked against the working directory
// it really names. That over-refuses a session-name candidate picked from
// inside a '#' directory, which is the safe direction. The home directory is
// looked up only for a candidate whose expansion starts with "~", and a failed
// lookup there is an error the caller refuses on, never a silently skipped
// check: a candidate that needs a home directory cannot be proven '#'-free
// without one, and a candidate that does not need one is still resolved.
func seshResolvedPaths(name string, userHomeDir func() (string, error)) ([]string, error) {
	path := os.ExpandEnv(name)
	if strings.HasPrefix(path, "~") {
		home, err := userHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", termsafe.Error(err))
		}
		path = strings.Replace(path, "~", home, 1)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil
	}
	paths := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		paths = append(paths, resolved)
	}
	return paths, nil
}
