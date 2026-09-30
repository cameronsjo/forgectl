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
				fmt.Fprintln(out, termsafe.SafeLine(n))
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
//   - sesh's namer resolves a directory candidate with EvalSymlinks and names
//     the session after the target, so a listed symlink pointing at a '#'
//     directory carries the '#' into -s. seshPick closes the part of this it
//     can see: an existing-path candidate is resolved the same way and refused
//     if the target contains '#'. The check is best-effort (the link can be
//     retargeted between the check and sesh's own resolution).
//   - sesh's git namer names a linked worktree after its main worktree's root,
//     and sesh's zoxide lookup fuzzy-matches a '#'-free query to a '#' path.
//     Neither is visible from here without re-implementing sesh's strategy
//     chain; only the upstream fix closes them (#841).
var errSeshUnsafeCandidate = errors.New("refusing to hand sesh a name containing '#': sesh passes it to tmux unescaped, where tmux would expand #(...) as a command")

// seshPick is the single forgectl-side gate in front of `sesh connect`. Both
// the `tmux pick <name>` command and the TUI picker's hand-off route through
// it.
func seshPick(ctx context.Context, client *tmux.Client, name string) error {
	if strings.Contains(name, "#") {
		return fmt.Errorf("%w: %s", errSeshUnsafeCandidate, termsafe.QuotePath(name))
	}
	if resolved, ok := resolveSeshPath(name); ok && strings.Contains(resolved, "#") {
		return fmt.Errorf("%w: %s resolves to %s", errSeshUnsafeCandidate,
			termsafe.QuotePath(name), termsafe.QuotePath(resolved))
	}
	return client.Pick(ctx, name)
}

// resolveSeshPath resolves a candidate the way sesh's namer does when the
// candidate names an existing path: a leading ~ expands to the home directory
// (sesh lists zoxide entries home-shortened), then EvalSymlinks. ok is false
// when the candidate is not an existing path, which is the session-name and
// zoxide-query case sesh resolves on its own.
func resolveSeshPath(name string) (string, bool) {
	path := name
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	return resolved, true
}
