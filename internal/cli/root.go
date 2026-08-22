// Package cli holds the thin Cobra verbs layered over the domain packages
// (internal/tmux, internal/projects, …). Commands parse flags and call ops;
// they hold no domain logic of their own. Command groups register through the
// module registry in modules.go (ADR-0005).
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// structuredTerminalError is composed only from trusted layout and fields
// sanitized at their trust boundary. termsafeErrorHandler recognizes this
// exact private type so it can preserve those newlines; every other error still
// passes through the one-physical-line fallback.
type structuredTerminalError struct {
	headline    string
	suggestions []string
}

func (e *structuredTerminalError) Error() string {
	if len(e.suggestions) == 0 {
		return e.headline
	}
	return e.headline + "\n\nDid you mean this?\n  " + strings.Join(e.suggestions, "\n  ")
}

// safeRootArgs mirrors Cobra's legacy root-argument validation while keeping
// the unknown verb and suggestions separate from Cobra-authored structure.
// Cobra otherwise concatenates all of them into one opaque error string, after
// which the error sink cannot tell a hostile embedded newline from its own
// "Did you mean" layout.
func safeRootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}

	headline := fmt.Sprintf("unknown command %s for %s", termsafe.QuoteText(args[0]), termsafe.QuoteText(cmd.CommandPath()))
	suggestions := cmd.SuggestionsFor(args[0])
	for i := range suggestions {
		suggestions[i] = termsafe.SafeLine(suggestions[i])
	}

	return &structuredTerminalError{headline: headline, suggestions: suggestions}
}

// showRootHelp keeps Cobra's prior non-runnable-root behavior after adding the
// Args validator: a bare headless invocation still prints help and returns nil
// to Execute, which turns the no-dispatch outcome into errHeadlessMenuRoute.
func showRootHelp(*cobra.Command, []string) error { return pflag.ErrHelp }

// newRoot builds the root command tree from the module registry
// (allModules) — every command group registers through its manifest
// (ADR-0005).
func newRoot(deps module.Deps) *cobra.Command {
	root := &cobra.Command{
		Use:     meta.AppName,
		Short:   meta.Tagline,
		Version: meta.Version,
		Args:    safeRootArgs,
		RunE:    showRootHelp,
		// fang renders styled errors/usage; we own when usage appears so an op
		// failure doesn't dump a wall of help. Bare-invoke → TUI is handled in
		// Execute, before Cobra runs.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// "did you mean" for fat-fingered verbs; the forgive layer handles the rest.
	root.SuggestionsMinimumDistance = 2
	// Honored by the TUI and the tree verb; swaps Nerd Font glyphs for ASCII.
	root.PersistentFlags().Bool("no-icons", false, "use ASCII markers instead of Nerd Font glyphs")

	for _, m := range allModules() {
		cmd := m.New(deps)
		// Append-if-absent: a constructor may already set its group alias in
		// its own literal (the ForClient test seams pin that surface), so the
		// manifest declaration must not duplicate it.
		for _, a := range m.GroupAliases {
			if !cmd.HasAlias(a) {
				cmd.Aliases = append(cmd.Aliases, a)
			}
		}
		// Deliberate re-application, not dead code: constructors with a
		// SubAliases surface also self-apply (their test seams need the
		// aliases), and applyAliases overwrites with the same map, so this
		// copy is the safety net for any constructor that doesn't.
		applyAliases(cmd, m.SubAliases)
		root.AddCommand(cmd)
	}

	// Bare `version` fell through to the TUI after the module refactor moved
	// version onto fang's --version flag only (ADR-0005); restore it as a
	// leaf verb outside the registry, alongside --version.
	root.AddCommand(newVersionCmd())

	return root
}
