package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/quarantine"
)

// quarantineFlags backs the flag set shared by the quarantine parent command
// and its hide/restore/status subverbs. Each command owns its own instance —
// no flag values are shared across commands.
type quarantineFlags struct {
	root    string
	scheme  string
	targets []string
	dryRun  bool
}

// register attaches the common quarantine flags to cmd. includeDryRun is
// false for status, which never mutates the filesystem.
func (f *quarantineFlags) register(cmd *cobra.Command, includeDryRun bool) {
	cmd.Flags().StringVar(&f.root, "root", "", "quarantine root directory (default: cwd)")
	cmd.Flags().StringVar(&f.scheme, "scheme", "prefix", "rename scheme: prefix or suffix")
	cmd.Flags().StringArrayVar(&f.targets, "targets", nil, "target path relative to root (repeatable; default: the canonical instruction-file list)")
	if includeDryRun {
		cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "report planned moves without renaming anything")
	}
}

// resolvedTargets returns f.targets, falling back to the canonical
// instruction-file list when the flag was omitted.
func (f *quarantineFlags) resolvedTargets() []string {
	if len(f.targets) == 0 {
		return quarantine.DefaultTargets
	}
	return f.targets
}

// resolveQuarantineRoot returns root, or the current working directory when
// root is empty (the --root flag's default).
func resolveQuarantineRoot(root string) (string, error) {
	if root != "" {
		return root, nil
	}
	return os.Getwd()
}

// newQuarantineCmd builds the `quarantine` parent command. Hide is the
// default action: a bare `forgectl quarantine` behaves exactly like
// `forgectl quarantine hide`.
func newQuarantineCmd(client *quarantine.Client) *cobra.Command {
	f := &quarantineFlags{}
	cmd := &cobra.Command{
		Use:   "quarantine",
		Short: "Reversibly hide AI-instruction files from a workspace",
		Long: `quarantine renames AI-instruction files (CLAUDE.md, AGENTS.md, …) aside via
os.Rename — reversible, unlike workflow's destructive strip step.

  forgectl quarantine                 hide the default targets in cwd
  forgectl quarantine hide            same, explicit
  forgectl quarantine restore         rename quarantined targets back
  forgectl quarantine status          show which targets are hidden

Targets default to the canonical instruction-file list; override with
repeatable --targets.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runQuarantineHide(cmd, client, f)
		},
	}
	f.register(cmd, true)
	cmd.AddCommand(
		newQuarantineHideCmd(client),
		newQuarantineRestoreCmd(client),
		newQuarantineStatusCmd(),
	)
	return cmd
}

// newQuarantineHideCmd builds `quarantine hide`.
func newQuarantineHideCmd(client *quarantine.Client) *cobra.Command {
	f := &quarantineFlags{}
	cmd := &cobra.Command{
		Use:   "hide",
		Short: "Rename AI-instruction files aside (reversible)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runQuarantineHide(cmd, client, f)
		},
	}
	f.register(cmd, true)
	return cmd
}

// newQuarantineRestoreCmd builds `quarantine restore`.
func newQuarantineRestoreCmd(client *quarantine.Client) *cobra.Command {
	f := &quarantineFlags{}
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Rename quarantined files back to their original names",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runQuarantineRestore(cmd, client, f)
		},
	}
	f.register(cmd, true)
	return cmd
}

// newQuarantineStatusCmd builds `quarantine status`. It only reads the
// filesystem, so it needs no *quarantine.Client.
func newQuarantineStatusCmd() *cobra.Command {
	f := &quarantineFlags{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show which instruction files are currently quarantined",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runQuarantineStatus(cmd, f)
		},
	}
	f.register(cmd, false)
	return cmd
}

// runQuarantineHide resolves f against cwd/DefaultTargets and calls
// client.Hide, printing each move (planned, on --dry-run, or performed).
func runQuarantineHide(cmd *cobra.Command, client *quarantine.Client, f *quarantineFlags) error {
	scheme, err := quarantine.ParseScheme(f.scheme)
	if err != nil {
		return err
	}
	root, err := resolveQuarantineRoot(f.root)
	if err != nil {
		return fmt.Errorf("resolve quarantine root: %w", err)
	}

	moves, err := client.Hide(cmd.Context(), root, scheme, f.resolvedTargets(), f.dryRun)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if len(moves) == 0 {
		fmt.Fprintln(out, "no instruction files found to quarantine")
		return nil
	}
	verb := "quarantined"
	if f.dryRun {
		verb = "would quarantine"
	}
	for _, m := range moves {
		fmt.Fprintf(out, "%s %s -> %s\n", verb, m.From, m.To)
	}
	return nil
}

// runQuarantineRestore recomputes the From/To mapping for f's targets and
// hands it to client.Restore — restore takes no persisted move list, so a
// restore call must use the same root/scheme/targets the original hide used.
func runQuarantineRestore(cmd *cobra.Command, client *quarantine.Client, f *quarantineFlags) error {
	scheme, err := quarantine.ParseScheme(f.scheme)
	if err != nil {
		return err
	}
	root, err := resolveQuarantineRoot(f.root)
	if err != nil {
		return fmt.Errorf("resolve quarantine root: %w", err)
	}

	moves, err := quarantine.ComputeMoves(root, scheme, f.resolvedTargets())
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if f.dryRun {
		for _, m := range moves {
			fmt.Fprintf(out, "would restore %s -> %s\n", m.To, m.From)
		}
		return nil
	}

	if err := client.Restore(cmd.Context(), moves); err != nil {
		return err
	}
	for _, m := range moves {
		fmt.Fprintf(out, "restored %s -> %s\n", m.To, m.From)
	}
	return nil
}

// runQuarantineStatus reports, per target, whether the original path is
// present, the quarantined form exists, or neither is found.
func runQuarantineStatus(cmd *cobra.Command, f *quarantineFlags) error {
	scheme, err := quarantine.ParseScheme(f.scheme)
	if err != nil {
		return err
	}
	root, err := resolveQuarantineRoot(f.root)
	if err != nil {
		return fmt.Errorf("resolve quarantine root: %w", err)
	}

	targets := f.resolvedTargets()
	moves, err := quarantine.ComputeMoves(root, scheme, targets)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	for i, m := range moves {
		state := "absent"
		switch {
		case pathExists(m.From):
			state = "present"
		case pathExists(m.To):
			state = "quarantined"
		}
		fmt.Fprintf(out, "%s: %s\n", targets[i], state)
	}
	return nil
}

// pathExists reports whether path exists (without following the final
// symlink, matching Hide/Restore's own os.Lstat checks).
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
