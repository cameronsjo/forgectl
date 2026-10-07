// Package cli holds the thin Cobra verbs layered over the domain packages
// (internal/tmux, internal/projects, …). Commands parse flags and call ops;
// they hold no domain logic of their own. Command groups register through the
// module registry in modules.go (ADR-0005).
package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// everydayGroupID and moreGroupID are root help's two cobra groups
// (ADR-0005 addendum): every module's GroupID follows its Tier directly, so
// promoting/demoting a module's tier (modules_test.go's core-set pin) moves
// its help placement for free.
const (
	everydayGroupID = "everyday"
	moreGroupID     = "more"
)

// hubOrderAnnotation and hubTierAnnotation are cobra Annotations keys
// buildHub (hub.go) reads back off root's constructed children to classify
// and order hub rows without calling allModules() itself — a
// module.Manifest.New closure that did (tmux's hub row needs the hub) would
// create a package initialization cycle back through tmuxModule's own var
// initializer. Annotations, not root help's GroupID: buildHub needs the
// tier and registry order regardless of whether root help's grouping ever
// changes shape, and the two are deliberately kept independent so a change
// to one cannot silently break the other.
//
// hubOrderAnnotation recovers each module's allModules() registry
// position — cobra's own Commands() getter sorts alphabetically by
// default, which would scramble the hub's required row order.
const (
	hubOrderAnnotation = "forgectl:hub-order"
	hubTierAnnotation  = "forgectl:hub-tier"
	hubTierCore        = "core"
	hubTierExtension   = "extension"
)

// hubNoPickerAnnotation is set by a command whose positional argument goes
// into another CLI's subcommand slot, so a typed value is never "just an
// argument" there: the hub must not offer its inline picker for it
// (forgectl#730). Its value says why; its presence is what counts. The hub
// rows built for such a command carry NoPicker and print the invocation
// instead, and hubPickerArgv refuses the command as a backstop.
//
// The variadic rule in tui.pickerSpec already keeps the picker off a command
// that takes a whole argv (launch); this annotation covers the one-positional
// shape that rule cannot see.
const hubNoPickerAnnotation = "forgectl:hub-no-picker"

// hasNoPickerAnnotation reports whether cmd opted out of the hub picker.
func hasNoPickerAnnotation(cmd *cobra.Command) bool {
	_, ok := cmd.Annotations[hubNoPickerAnnotation]
	return ok
}

// stampHubAnnotations records a module's registry position and tier on its
// command for buildHub. It merges into the constructor's own annotations
// rather than replacing them, so a module root keeps any it set itself
// (hubNoPickerAnnotation among them).
func stampHubAnnotations(cmd *cobra.Command, order int, tier string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[hubOrderAnnotation] = strconv.Itoa(order)
	cmd.Annotations[hubTierAnnotation] = tier
}

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

	headline := fmt.Sprintf("unknown command %s for %s", termsafe.QuoteArgMax(args[0], termsafe.ArgEchoMaxRunes), termsafe.QuoteArgMax(cmd.CommandPath(), termsafe.ArgEchoMaxRunes))
	suggestions := cmd.SuggestionsFor(args[0])
	for i := range suggestions {
		suggestions[i] = safeLabel(suggestions[i])
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
		Use:   meta.AppName,
		Short: meta.Tagline,
		Long: `Two ways in: type a command — forgectl tmux ls — or run forgectl with no
arguments for a menu over every command group.

Exit codes: 0 ok, 1 failed, 2 usage, 3 unauthorized, 4 refused; see docs/exit-codes.md.`,
		Version: meta.Version,
		Args:    safeRootArgs,
		RunE:    rootRun,
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
	addSkillFlags(root)

	root.AddGroup(
		&cobra.Group{ID: everydayGroupID, Title: "Everyday:"},
		&cobra.Group{ID: moreGroupID, Title: "More:"},
	)

	for i, m := range allModules() {
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
		tier := hubTierCore
		cmd.GroupID = everydayGroupID
		if m.Tier == module.TierExtension {
			tier = hubTierExtension
			cmd.GroupID = moreGroupID
		}
		stampHubAnnotations(cmd, i, tier)
		root.AddCommand(cmd)
	}

	// Bare `version` fell through to the TUI after the module refactor moved
	// version onto fang's --version flag only (ADR-0005); restore it as a
	// leaf verb outside the registry, alongside --version.
	root.AddCommand(newVersionCmd())

	// `menu` prints the hub's contents without a TTY (forgectl#730 item 5).
	// Outside the registry like version: it describes the hub, so it has no
	// hub tier and no row of its own.
	root.AddCommand(newMenuCmd(deps))

	// The editor `env set --sops` points sops at — forgectl re-invoking
	// itself. Registered outside the registry because it is not a verb anyone
	// runs: it is half of an internal protocol, and its own guards (not its
	// Hidden flag) are what make it safe to expose at all.
	root.AddCommand(newSopsEditCmd())

	rejectUnknownSubcommands(root)

	// After the group pass, before the JSON contract wraps Args: a wrong argument
	// count names the argument and the usage line (forgectl#1087).
	nameUsageArgs(root)

	// Before the JSON contract, so a usage error carries its exit code into it
	// (ADR-0015).
	classifyUsageErrors(root)

	// Last, so it sees every verb: under --json no failure renders fang's
	// human error frame (forgectl#862, json_errors.go).
	installJSONErrorContract(root)

	return root
}
