package cli

import (
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// Every test below builds a real root over the full module registry — both
// tiers are present in allModules() today (31 modules, 6 core), so this
// exercises buildHub against real cobra Short/Use text rather than a
// hand-rolled tree.

func TestBuildHub_FirstRunRow(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, false, nil)
	if len(entries) == 0 {
		t.Fatal("buildHub returned no entries")
	}
	got := entries[0]
	if got.Name != "init" {
		t.Fatalf("entries[0].Name = %q, want %q", got.Name, "init")
	}
	wantShort := "first run: set up forgectl — creates config.toml (init)"
	if got.Short != wantShort {
		t.Errorf("entries[0].Short = %q, want %q", got.Short, wantShort)
	}
}

func TestBuildHub_NoFirstRunRowWhenConfigPresent(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true, nil)
	if len(entries) == 0 {
		t.Fatal("buildHub returned no entries")
	}
	if entries[0].Name == "init" {
		t.Error("entries[0] is the first-run row even though configPresent=true")
	}
}

// hubNames lists entries' names, dividers included.
func hubNames(entries []tui.HubEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// TestBuildHub_PinOrder pins forgectl#730 item 2: the five pinned commands
// first, in their fixed order, then an "all commands (N)" divider whose N is
// the number of module rows under it, then every remaining module in
// registry order — none of them repeating a pinned row.
func TestBuildHub_PinOrder(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true, nil)
	names := hubNames(entries)

	want := []string{"docs", "pr", "projects", "tmux", "sessions"}
	if len(names) < len(want)+1 {
		t.Fatalf("got %d entries, want at least %d: %v", len(names), len(want)+1, names)
	}
	for i, w := range want {
		if names[i] != w || entries[i].Heading {
			t.Errorf("entries[%d] = %q (heading=%v), want pinned %q (full order: %v)", i, names[i], entries[i].Heading, w, names)
		}
	}

	div := entries[len(want)]
	rest := entries[len(want)+1:]
	if !div.Heading || div.Name != "all commands ("+strconv.Itoa(len(rest))+")" {
		t.Fatalf("entries[%d] = %+v, want the divider \"all commands (%d)\"", len(want), div, len(rest))
	}

	var wantRest []string
	pinned := map[string]bool{}
	for _, w := range want {
		pinned[w] = true
	}
	for _, m := range allModules() {
		if !pinned[m.Name] {
			wantRest = append(wantRest, m.Name)
		}
	}
	gotRest := hubNames(rest)
	if strings.Join(gotRest, ",") != strings.Join(wantRest, ",") {
		t.Errorf("rows under the divider =\n  %v\nwant registry order without the pinned five =\n  %v", gotRest, wantRest)
	}
	for _, e := range rest {
		if e.Heading {
			t.Errorf("unexpected divider %q among the module rows", e.Name)
		}
	}
}

// TestBuildHub_RecentSectionSitsBetweenPinsAndAll pins where the recent rows
// go and what they carry: a "recent" divider right after the pinned five, one
// direct-command row per recent command with its full argv, then the
// all-commands divider.
func TestBuildHub_RecentSectionSitsBetweenPinsAndAll(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	prs, _, err := root.Find([]string{"pr", "prs"})
	if err != nil {
		t.Fatal(err)
	}
	last, _, err := root.Find([]string{"sessions", "last"})
	if err != nil {
		t.Fatal(err)
	}
	entries := buildHub(root, true, []*cobra.Command{prs, last})
	names := hubNames(entries)

	if len(entries) < 9 {
		t.Fatalf("too few entries: %v", names)
	}
	if !entries[5].Heading || entries[5].Name != "recent" {
		t.Fatalf("entries[5] = %+v, want the \"recent\" divider (order: %v)", entries[5], names)
	}
	if got := strings.Join(entries[6].Argv, " "); got != "pr prs" || entries[6].NeedsArgs {
		t.Errorf("recent row 1 = %+v, want argv [pr prs] with no argument", entries[6])
	}
	if got := strings.Join(entries[7].Argv, " "); got != "sessions last" || !entries[7].NeedsArgs || entries[7].Use != last.Use {
		t.Errorf("recent row 2 = %+v, want argv [sessions last], NeedsArgs, Use %q", entries[7], last.Use)
	}
	if !entries[8].Heading || !strings.HasPrefix(entries[8].Name, "all commands (") {
		t.Errorf("entries[8] = %+v, want the all-commands divider", entries[8])
	}
}

// TestBuildHub_FirstRunRowPrecedesPins keeps the first-run row on top.
func TestBuildHub_FirstRunRowPrecedesPins(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	names := hubNames(buildHub(root, false, nil))
	if len(names) < 2 || names[0] != "init" || names[1] != "docs" {
		t.Errorf("order = %v, want init then docs", names)
	}
}

// TestBuildHub_PrLeafNeedsArgs pins the NeedsArgs leaf pr contributes to its
// own hub row: pr's bare invocation needs a <ref>, so buildLeaves adds a
// synthetic leaf named "pr" with NeedsArgs true and its Use line intact.
func TestBuildHub_PrLeafNeedsArgs(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true, nil)

	var pr *tui.HubEntry
	for i := range entries {
		if entries[i].Name == "pr" {
			pr = &entries[i]
		}
	}
	if pr == nil {
		t.Fatal("no \"pr\" entry in the hub")
	}

	var leaf *tui.HubLeaf
	for i := range pr.Leaves {
		if pr.Leaves[i].Name == "pr" {
			leaf = &pr.Leaves[i]
		}
	}
	if leaf == nil {
		t.Fatalf("pr entry has no synthetic \"pr\" leaf for its own <ref> requirement: %+v", pr.Leaves)
	}
	if !leaf.NeedsArgs {
		t.Error("pr's own leaf has NeedsArgs = false, want true")
	}
	if leaf.Use != "pr <ref>" {
		t.Errorf("pr's own leaf Use = %q, want %q", leaf.Use, "pr <ref>")
	}
}

// TestBuildHub_LeaflessExtensionRunsDirectly pins the general rule
// (Architecture: "a module with no leaves, e.g. doctor, runs directly") at
// the data level: doctor is its own row under "all commands", with no leaves.
func TestBuildHub_LeaflessExtensionRunsDirectly(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true, nil)

	var doctor *tui.HubEntry
	for i := range entries {
		if entries[i].Name == "doctor" {
			doctor = &entries[i]
		}
	}
	if doctor == nil {
		t.Fatalf("no \"doctor\" row in the hub: %v", hubNames(entries))
	}
	if len(doctor.Leaves) != 0 || doctor.Argv != nil || doctor.Heading {
		t.Errorf("doctor row = %+v, want a leafless module row (it runs directly)", doctor)
	}
}

// TestBuildHub_NestedGroupsCarryTheirLeaves pins #916: a subverb that is
// itself a group (pr findings, pr reviewed) carries its own subverbs as
// Leaves, at every depth, so the hub opens it rather than running it bare.
// Every leaf is checked against the live command it names: Leaves is present
// exactly when that command has available subcommands.
func TestBuildHub_NestedGroupsCarryTheirLeaves(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true, nil)

	var check func(cmd *cobra.Command, leaves []tui.HubLeaf, path string)
	check = func(cmd *cobra.Command, leaves []tui.HubLeaf, path string) {
		for _, leaf := range leaves {
			if leaf.Name == cmd.Name() {
				continue // the synthetic self leaf
			}
			sub := findChild(cmd, leaf.Name)
			if sub == nil {
				t.Errorf("%s: leaf %q names no subcommand", path, leaf.Name)
				continue
			}
			isGroup := sub.HasAvailableSubCommands()
			if isGroup != (len(leaf.Leaves) > 0) {
				t.Errorf("%s %s: group=%t but leaf has %d leaves", path, leaf.Name, isGroup, len(leaf.Leaves))
			}
			if isGroup && leaf.NeedsArgs {
				t.Errorf("%s %s: a group row must open its leaves, not ask for an argument", path, leaf.Name)
			}
			check(sub, leaf.Leaves, path+" "+leaf.Name)
		}
	}
	checked := 0
	for _, e := range entries {
		if e.Heading || e.Argv != nil {
			continue
		}
		if cmd := findChild(root, e.Name); cmd != nil {
			check(cmd, e.Leaves, e.Name)
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no module rows were checked")
	}

	var pr *tui.HubEntry
	for i := range entries {
		if entries[i].Name == "pr" && entries[i].Argv == nil {
			pr = &entries[i]
		}
	}
	if pr == nil {
		t.Fatal("no \"pr\" entry in the hub")
	}
	want := map[string][]string{"findings": {"cleanup", "list"}, "reviewed": {"mark", "sync", "unmark"}}
	for _, leaf := range pr.Leaves {
		names, ok := want[leaf.Name]
		if !ok {
			continue
		}
		delete(want, leaf.Name)
		var got []string
		for _, l := range leaf.Leaves {
			got = append(got, l.Name)
		}
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("pr %s leaves = %v, want %v", leaf.Name, got, names)
		}
	}
	if len(want) != 0 {
		t.Errorf("pr is missing group leaves %v", want)
	}
}
