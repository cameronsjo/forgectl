// Package cli holds the thin Cobra verbs layered over the domain packages
// (internal/tmux, internal/projects, …). Commands parse flags and call ops;
// they hold no domain logic of their own. Command groups register through the
// module registry in modules.go (ADR-0005).
package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// pinnedGroupID is root help's first cobra group: the hub's pinned commands
// (hubPinned). Every other module goes under the hub area that holds it
// (hubGroups), so root help and the menu sort commands the same way
// (forgectl#1151). A module no area names goes under hubOtherGroup, as it
// does on the hub.
const pinnedGroupID = "pinned"

// rootHelpGroupID is the root help group a module named name sits in.
func rootHelpGroupID(name string) string {
	if slices.Contains(hubPinned, name) {
		return pinnedGroupID
	}
	for _, g := range hubGroups {
		if slices.Contains(g.names, name) {
			return g.title
		}
	}
	return hubOtherGroup
}

// rootHelpGroups is root help's cobra groups, in the hub's order. fang skips
// a group with no command in it.
func rootHelpGroups() []*cobra.Group {
	groups := []*cobra.Group{{ID: pinnedGroupID, Title: "Pinned:"}}
	for _, g := range hubGroups {
		groups = append(groups, &cobra.Group{ID: g.title, Title: strings.ToUpper(g.title[:1]) + g.title[1:] + ":"})
	}
	return append(groups, &cobra.Group{ID: hubOtherGroup, Title: "Other:"})
}

// hubOrderAnnotation and hubTierAnnotation are cobra Annotations keys
// buildHub (hub.go) reads back off root's constructed children to classify
// and order hub rows without calling allModules() itself — a
// module.Manifest.New closure that did (tmux's hub row needs the hub) would
// create a package initialization cycle back through tmuxModule's own var
// initializer. Annotations, not root help's GroupID: root help's groups are
// derived from the hub's own lists (rootHelpGroupID), while the tier is
// ADR-0005's growth policy and no longer decides help placement, so the
// hub reads tier and registry order from here.
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

// rootHelpUseAnnotation gives a command a shorter usage for root help's
// command list only. fang pads that column to the widest usage, so one long
// Use line (review's flag placeholders) widened every row. The command's own
// Use, which its help page, the hub and `menu` read, is unchanged
// (forgectl#1151). withRootHelpLayout applies it.
const rootHelpUseAnnotation = "forgectl:root-help-use"

// withRootHelpLayout runs fn, which renders root's own help page, with that
// page laid out like the hub (forgectl#1151): each child carrying
// rootHelpUseAnnotation shows its short Use, and root's children are listed
// in rootHelpOrder rather than alphabetically. fang lists a group's commands
// in root.Commands() order, which cobra sorts unless EnableCommandSorting is
// off, so sorting is switched off and the children re-added in order for the
// length of fn. Everything is restored after: every Use, and the setting, so
// the next Commands() call sorts again.
func withRootHelpLayout(root *cobra.Command, fn func()) {
	type swap struct {
		cmd *cobra.Command
		use string
	}
	var swaps []swap
	for _, c := range root.Commands() {
		if short, ok := c.Annotations[rootHelpUseAnnotation]; ok {
			swaps = append(swaps, swap{c, c.Use})
			c.Use = short
		}
	}
	prevSorting := cobra.EnableCommandSorting
	ordered := rootHelpOrder(root)
	root.RemoveCommand(ordered...)
	cobra.EnableCommandSorting = false
	root.AddCommand(ordered...)
	// AddCommand recomputed root's commandsMaxUseLen from the short Uses, and
	// it stays at that width after the Uses are restored. Only cobra's default
	// usage template reads it (UsagePadding); fang renders its own help and
	// nothing else in forgectl reads it.
	defer func() {
		cobra.EnableCommandSorting = prevSorting
		for _, s := range swaps {
			s.cmd.Use = s.use
		}
	}()
	fn()
}

// rootHelpOrder is root's children in the hub's order: the pinned commands in
// hubPinned order, then each area's commands in hubGroups order, then every
// other child (version, menu, anything no area names) as cobra sorts them.
func rootHelpOrder(root *cobra.Command) []*cobra.Command {
	children := root.Commands()
	byName := make(map[string]*cobra.Command, len(children))
	for _, c := range children {
		byName[c.Name()] = c
	}
	out := make([]*cobra.Command, 0, len(children))
	placed := make(map[*cobra.Command]bool, len(children))
	add := func(name string) {
		if c, ok := byName[name]; ok && !placed[c] {
			out = append(out, c)
			placed[c] = true
		}
	}
	for _, name := range hubPinned {
		add(name)
	}
	for _, g := range hubGroups {
		for _, name := range g.names {
			add(name)
		}
	}
	for _, c := range children {
		if !placed[c] {
			out = append(out, c)
		}
	}
	return out
}

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
//
// With --skill set, a positional is a reference name, which runRootSkill
// validates.
func safeRootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if wantSkill, _ := cmd.Flags().GetBool("skill"); wantSkill {
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
		Long: `forgectl: your workbench from one command.

Type a command (forgectl status) or run forgectl alone for the menu.
New here? forgectl init, then forgectl doctor.
Agents: state verbs take --json; forgectl menu --json <command> lists its
verbs. Exit codes: 0 ok, 1 failed, 2 usage, 3 unauthorized, 4 refused
(docs/exit-codes.md).`,
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

	root.AddGroup(rootHelpGroups()...)

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
		// The tier is recorded for the hub; help placement follows the hub's
		// sections, not the tier (forgectl#1151).
		tier := hubTierCore
		if m.Tier == module.TierExtension {
			tier = hubTierExtension
		}
		cmd.GroupID = rootHelpGroupID(cmd.Name())
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
