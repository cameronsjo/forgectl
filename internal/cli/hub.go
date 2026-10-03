package cli

import (
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/history"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// hubPinned is the hub's first section (forgectl#730 item 2): the commands
// the hub most often routes to, in this order, ahead of everything else.
var hubPinned = []string{"docs", "pr", "projects", "tmux", "sessions"}

// hubRecentLimit is how many rows the "recent" section shows, and
// hubRecentWindow how many of the newest forgectl lines in shell history rank
// them.
const (
	hubRecentLimit  = 3
	hubRecentWindow = 200
)

// hubSections is the hub's content before layout (forgectl#730): whether the
// first-run row shows, the pinned modules in hubPinned order, the recent rows,
// and every remaining module in registry order. buildHub lays it out for the
// TUI and `menu` reports it, so the two cannot disagree about what the hub
// holds.
type hubSections struct {
	firstRun bool
	pinned   []tui.HubEntry
	recent   []tui.HubEntry
	rest     []tui.HubEntry
}

// collectHubSections derives the hub's sections from the live cobra tree
// (ADR-0005 addendum): no new manifest field, no Menu hook. Each row's
// Name/Short come straight off root's constructed children; tier and registry
// order are read back off the hubTierAnnotation and hubOrderAnnotation root.go
// stamped onto each command at construction — it deliberately never calls
// allModules() itself, because a module.Manifest.New closure that did (tmux's
// hub row needs the hub) would create a package initialization cycle back
// through tmuxModule's own var initializer.
func collectHubSections(root *cobra.Command, configPresent bool, recent []*cobra.Command) hubSections {
	sec := hubSections{firstRun: !configPresent}
	modules := hubModules(root)
	byName := make(map[string]*cobra.Command, len(modules))
	for _, child := range modules {
		byName[child.Name()] = child
	}
	pinned := make(map[string]bool, len(hubPinned))
	for _, name := range hubPinned {
		if child, ok := byName[name]; ok {
			sec.pinned = append(sec.pinned, moduleEntry(child))
			pinned[name] = true
		}
	}
	for _, cmd := range recent {
		sec.recent = append(sec.recent, recentEntry(cmd))
	}
	for _, child := range modules {
		if !pinned[child.Name()] {
			sec.rest = append(sec.rest, moduleEntry(child))
		}
	}
	return sec
}

// buildHub lays out the hub's rows (collectHubSections) for the TUI: an
// optional first-run row when configPresent is false; the pinned commands
// (hubPinned) in their fixed order; a "recent" divider and one row per recent
// command when there are any; then an "all commands (N)" divider over every
// remaining module in registry order.
func buildHub(root *cobra.Command, configPresent bool, recent []*cobra.Command) []tui.HubEntry {
	sec := collectHubSections(root, configPresent, recent)
	var entries []tui.HubEntry
	if sec.firstRun {
		entries = append(entries, tui.HubEntry{
			Name:  "init",
			Short: hubInitShort,
			Core:  true,
		})
	}
	entries = append(entries, sec.pinned...)
	if len(sec.recent) > 0 {
		entries = append(entries, tui.HubEntry{Name: "recent", Heading: true})
		entries = append(entries, sec.recent...)
	}
	if len(sec.rest) > 0 {
		entries = append(entries, tui.HubEntry{Name: "all commands (" + strconv.Itoa(len(sec.rest)) + ")", Heading: true})
		entries = append(entries, sec.rest...)
	}
	return entries
}

// hubInitShort is the first-run row's description.
const hubInitShort = "first run: set up forgectl — creates config.toml (init)"

// hubModules returns root's registered modules — both tiers — in registry
// order. A command with no tier annotation (version, a hidden helper) is not
// a module and never gets a row.
func hubModules(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, child := range root.Commands() {
		if isHubModule(child) {
			out = append(out, child)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return hubOrder(out[i]) < hubOrder(out[j])
	})
	return out
}

// isHubModule reports whether a root child is a registered module (either
// tier), the only commands the hub has rows for.
func isHubModule(cmd *cobra.Command) bool {
	switch cmd.Annotations[hubTierAnnotation] {
	case hubTierCore, hubTierExtension:
		return true
	}
	return false
}

// topLevel is cmd's ancestor directly under root (cmd itself at depth one).
func topLevel(root, cmd *cobra.Command) *cobra.Command {
	for cmd.HasParent() && cmd.Parent() != root {
		cmd = cmd.Parent()
	}
	return cmd
}

// moduleEntry is one module's hub row.
func moduleEntry(child *cobra.Command) tui.HubEntry {
	return tui.HubEntry{
		Name:     child.Name(),
		Short:    child.Short,
		Core:     child.Annotations[hubTierAnnotation] == hubTierCore,
		Use:      child.Use,
		Leaves:   buildLeaves(child),
		NoPicker: hasNoPickerAnnotation(child),
	}
}

// recentEntry is one "recent" row: a runnable command path run directly, or
// through the argument picker when its Use names an argument.
func recentEntry(cmd *cobra.Command) tui.HubEntry {
	argv := commandArgv(cmd)
	return tui.HubEntry{
		Name:      strings.Join(argv, " "),
		Short:     cmd.Short,
		Use:       cmd.Use,
		Argv:      argv,
		NeedsArgs: parentTakesArg(cmd),
		NoPicker:  hasNoPickerAnnotation(cmd),
	}
}

// commandArgv is cmd's path below root, as argv ("pr", "prs").
func commandArgv(cmd *cobra.Command) []string {
	var argv []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		argv = append([]string{c.Name()}, argv...)
	}
	return argv
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
//
// A subverb that is itself a group (pr findings, pr reviewed) gets its own
// subverbs as Leaves, built by the same rule, so selecting it opens them
// instead of running the group bare (#916). Such a row's own positional, if
// it has one, is the synthetic leaf inside it, so the row itself never needs
// an argument.
func buildLeaves(cmd *cobra.Command) []tui.HubLeaf {
	var leaves []tui.HubLeaf
	if parentTakesArg(cmd) {
		leaves = append(leaves, tui.HubLeaf{Name: cmd.Name(), Short: cmd.Short, Use: cmd.Use, NeedsArgs: true, Self: true, NoPicker: hasNoPickerAnnotation(cmd)})
	}
	for _, sub := range cmd.Commands() {
		if !sub.IsAvailableCommand() {
			continue
		}
		var nested []tui.HubLeaf
		if sub.HasAvailableSubCommands() {
			nested = buildLeaves(sub)
		}
		leaves = append(leaves, tui.HubLeaf{
			Name:      sub.Name(),
			Short:     sub.Short,
			Use:       sub.Use,
			NeedsArgs: len(nested) == 0 && parentTakesArg(sub),
			Leaves:    nested,
			NoPicker:  hasNoPickerAnnotation(sub),
		})
	}
	return leaves
}

// recentCommands ranks the runnable forgectl commands in shell history
// (forgectl#730 item 2): among the newest hubRecentWindow lines that invoke
// forgectl, by how often each command path appears, ties going to the most
// recent. Only the command path resolved against root's registered tree is
// kept, and only under a registered module (menu and version are not hub
// rows) — never the line's arguments or any other history text — so nothing
// the history file holds reaches the screen. A pinned module's bare
// invocation is left out: it already has a row.
func recentCommands(root *cobra.Command, entries []history.Entry, limit int) []*cobra.Command {
	type tally struct {
		cmd   *cobra.Command
		count int
		last  int
	}
	pinned := make(map[string]bool, len(hubPinned))
	for _, name := range hubPinned {
		pinned[name] = true
	}
	byPath := map[*cobra.Command]*tally{}
	scanned := 0
	for i := len(entries) - 1; i >= 0 && scanned < hubRecentWindow; i-- {
		cmd, isForgectl := historyCommand(root, entries[i].Command)
		if !isForgectl {
			continue
		}
		scanned++
		if cmd == nil {
			continue
		}
		if cmd.Parent() == root && pinned[cmd.Name()] {
			continue
		}
		if !isHubModule(topLevel(root, cmd)) {
			continue // menu, version: host plumbing, never a hub row
		}
		t := byPath[cmd]
		if t == nil {
			t = &tally{cmd: cmd, last: i}
			byPath[cmd] = t
		}
		t.count++
	}
	ranked := make([]*tally, 0, len(byPath))
	for _, t := range byPath {
		ranked = append(ranked, t)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].last > ranked[j].last
	})
	var out []*cobra.Command
	for _, t := range ranked {
		if len(out) == limit {
			break
		}
		out = append(out, t.cmd)
	}
	return out
}

// historyCommand resolves one history line. isForgectl reports whether the
// line invokes forgectl at all (its first word is forgectl or a path ending
// in /forgectl); cmd is the deepest registered, available, runnable command
// its leading non-flag words name, or nil when they name none.
func historyCommand(root *cobra.Command, line string) (cmd *cobra.Command, isForgectl bool) {
	first, _, _ := strings.Cut(line, "\n")
	fields := strings.Fields(first)
	if len(fields) == 0 {
		return nil, false
	}
	if fields[0] != meta.AppName && !strings.HasSuffix(fields[0], "/"+meta.AppName) {
		return nil, false
	}
	cur := root
	for _, tok := range fields[1:] {
		if strings.HasPrefix(tok, "-") {
			break
		}
		child := findChild(cur, tok)
		if child == nil || !child.IsAvailableCommand() {
			break
		}
		cur = child
	}
	if cur == root || !cur.Runnable() {
		return nil, true
	}
	return cur, true
}

// readShellHistory loads the operator's shell history for recentCommands. A
// missing or refused file yields nil: the recent section is best-effort and
// its absence is not an error the hub reports.
func readShellHistory() []history.Entry {
	path, err := history.ResolvePath(os.Getenv, os.UserHomeDir)
	if err != nil {
		return nil
	}
	entries, err := history.Read(path)
	if err != nil {
		slog.Debug("Hub skipped the recent section; shell history is unreadable.", "error", err)
		return nil
	}
	return entries
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
