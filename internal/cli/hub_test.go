package cli

import (
	"strconv"
	"strings"
	"testing"

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
	entries := buildHub(root, false)
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
	entries := buildHub(root, true)
	if len(entries) == 0 {
		t.Fatal("buildHub returned no entries")
	}
	if entries[0].Name == "init" {
		t.Error("entries[0] is the first-run row even though configPresent=true")
	}
}

// TestBuildHub_RowOrder pins the Architecture's row order: tmux first, then
// the remaining core-tier modules in registry order, then the all-commands
// aggregate last. Registry order (modules.go) happens to already list every
// core module before any extension module, so this also proves buildHub
// isn't re-sorting by name.
func TestBuildHub_RowOrder(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true)

	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"tmux", "projects", "config", "launch", "workflow", "pr"}
	if len(names) < len(want)+1 {
		t.Fatalf("got %d entries, want at least %d (core rows + aggregate): %v", len(names), len(want)+1, names)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("entries[%d].Name = %q, want %q (full order: %v)", i, names[i], w, names)
		}
	}
	last := names[len(names)-1]
	if !strings.HasPrefix(last, "all commands (") {
		t.Errorf("last entry = %q, want an \"all commands (N)\" aggregate row", last)
	}
}

// TestBuildHub_AllCommandsLabelHasExtensionCount pins the N in
// "all commands (N)" — Architecture: N = len(allModules()) - core.
func TestBuildHub_AllCommandsLabelHasExtensionCount(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true)
	last := entries[len(entries)-1]

	wantN := 0
	for _, m := range allModules() {
		if m.Tier == module.TierExtension {
			wantN++
		}
	}
	if !strings.Contains(last.Name, "("+strconv.Itoa(wantN)+")") {
		t.Errorf("aggregate row Name = %q, want it to contain (%d)", last.Name, wantN)
	}
	if !strings.HasSuffix(last.Short, "type to filter") {
		t.Errorf("aggregate row Short = %q, want it to end with the filter hint", last.Short)
	}
}

// TestBuildHub_PrLeafNeedsArgs pins the NeedsArgs leaf pr contributes to its
// own hub row: pr's bare invocation needs a <ref>, so buildLeaves adds a
// synthetic leaf named "pr" with NeedsArgs true and its Use line intact.
func TestBuildHub_PrLeafNeedsArgs(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true)

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
// the data level: doctor's flattened all-commands leaf carries its own name
// with no further subverb, and NeedsArgs is false.
func TestBuildHub_LeaflessExtensionRunsDirectly(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := buildHub(root, true)
	agg := entries[len(entries)-1]

	var doctor *tui.HubLeaf
	for i := range agg.Leaves {
		if agg.Leaves[i].Name == "doctor" {
			doctor = &agg.Leaves[i]
		}
	}
	if doctor == nil {
		t.Fatalf("no \"doctor\" leaf in the all-commands aggregate: %+v", agg.Leaves)
	}
	if doctor.NeedsArgs {
		t.Error("doctor leaf has NeedsArgs = true, want false (it runs directly, no args)")
	}
}
