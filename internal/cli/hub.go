package cli

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// hubExtensionSampleSize is how many extension-tier module names the
// "all commands" row's label previews (Architecture: "the sample is the
// first five extension names").
const hubExtensionSampleSize = 5

// buildHub derives the hub's rows from the live cobra tree (ADR-0005
// addendum): no new manifest field, no Menu hook. Each row's Name/Short come
// straight off root's constructed children; core-vs-extension and registry
// order are read back off the hubTierAnnotation and hubOrderAnnotation
// root.go stamped onto each command at construction — buildHub deliberately
// never calls allModules() itself, because a module.Manifest.New closure
// that did (tmux's hub row needs the hub) would create a package
// initialization cycle back through tmuxModule's own var initializer.
//
// Row order: an optional first-run row when configPresent is false, then
// tmux, then the remaining core-tier modules in registry order, then one
// "all commands" aggregate row over the extension tier.
func buildHub(root *cobra.Command, configPresent bool) []tui.HubEntry {
	var entries []tui.HubEntry
	if !configPresent {
		entries = append(entries, tui.HubEntry{
			Name:  "init",
			Short: "first run: set up forgectl — creates config.toml (init)",
			Core:  true,
		})
	}

	core := orderedChildren(root, hubTierCore)

	var tmuxEntry *tui.HubEntry
	var coreRest []tui.HubEntry
	for _, child := range core {
		e := tui.HubEntry{Name: child.Name(), Short: child.Short, Core: true, Leaves: buildLeaves(child)}
		if child.Name() == "tmux" {
			tmuxEntry = &e
			continue
		}
		coreRest = append(coreRest, e)
	}
	if tmuxEntry != nil {
		entries = append(entries, *tmuxEntry)
	}
	entries = append(entries, coreRest...)

	if agg, ok := buildAllCommandsEntry(root); ok {
		entries = append(entries, agg)
	}

	return entries
}

// orderedChildren returns root's direct children whose hubTierAnnotation is
// tier, sorted by the registry position root.go stamped on each (cobra's
// own Commands() sorts alphabetically, which would scramble "registry
// order").
func orderedChildren(root *cobra.Command, tier string) []*cobra.Command {
	var out []*cobra.Command
	for _, child := range root.Commands() {
		if child.Annotations[hubTierAnnotation] == tier {
			out = append(out, child)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return hubOrder(out[i]) < hubOrder(out[j])
	})
	return out
}

// hubOrder recovers a command's allModules() registry position from the
// annotation root.go stamped on it; a command missing the annotation
// (should not happen for a registered module) sorts last rather than
// panicking.
func hubOrder(cmd *cobra.Command) int {
	raw, ok := cmd.Annotations[hubOrderAnnotation]
	if !ok {
		return len(cmd.Root().Commands()) + 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return len(cmd.Root().Commands()) + 1
	}
	return n
}

// buildLeaves returns cmd's runnable subverbs as HubLeaf rows, plus — when
// cmd's OWN invocation needs a positional argument (parentTakesArg) — one
// synthetic leaf naming cmd itself, so a NeedsArgs module like pr (bare
// `forgectl pr <ref>`) still surfaces something to select from its hub row.
func buildLeaves(cmd *cobra.Command) []tui.HubLeaf {
	var leaves []tui.HubLeaf
	if parentTakesArg(cmd) {
		leaves = append(leaves, tui.HubLeaf{Name: cmd.Name(), Short: cmd.Short, Use: cmd.Use, NeedsArgs: true})
	}
	for _, sub := range cmd.Commands() {
		if !sub.IsAvailableCommand() {
			continue
		}
		leaves = append(leaves, tui.HubLeaf{
			Name:      sub.Name(),
			Short:     sub.Short,
			Use:       sub.Use,
			NeedsArgs: parentTakesArg(sub),
		})
	}
	return leaves
}

// buildAllCommandsEntry flattens the extension tier into one HubEntry whose
// Leaves are already complete argvs (space-joined) rather than single
// tokens: a leafless extension module (doctor) contributes one leaf named
// after itself, and a module with its own subverbs (docker) contributes one
// leaf per subverb named "<module> <subverb>". tui's leafArgv/usageLine
// split on that space to recover the real argv (hubAllCommandsPrefix there).
func buildAllCommandsEntry(root *cobra.Command) (tui.HubEntry, bool) {
	extensions := orderedChildren(root, hubTierExtension)
	if len(extensions) == 0 {
		return tui.HubEntry{}, false
	}

	var leaves []tui.HubLeaf
	var sample []string
	for _, child := range extensions {
		if len(sample) < hubExtensionSampleSize {
			sample = append(sample, child.Name())
		}
		subLeaves := buildLeaves(child)
		if len(subLeaves) == 0 {
			leaves = append(leaves, tui.HubLeaf{Name: child.Name(), Short: child.Short, Use: child.Use, NeedsArgs: parentTakesArg(child)})
			continue
		}
		for _, l := range subLeaves {
			name := child.Name()
			if l.Name != child.Name() {
				name = child.Name() + " " + l.Name
			}
			leaves = append(leaves, tui.HubLeaf{Name: name, Short: l.Short, Use: l.Use, NeedsArgs: l.NeedsArgs})
		}
	}

	return tui.HubEntry{
		Name:   "all commands (" + strconv.Itoa(len(extensions)) + ")",
		Short:  strings.Join(sample, " · ") + " — type to filter",
		Leaves: leaves,
	}, true
}

// configFilePresent reports whether config.toml exists at its expected
// path — buildHub's signal for the first-run row. A resolution failure
// (an unreadable config dir) is treated as "not present": the hub still
// opens, offering init rather than erroring on a menu render.
func configFilePresent() bool {
	path, err := config.ConfigPath()
	if err != nil {
		return false
	}
	_, statErr := os.Stat(path)
	return statErr == nil
}
