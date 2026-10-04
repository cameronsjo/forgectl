package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tui"
)

var updateMenuGolden = flag.Bool("update-menu-golden", false, "rewrite internal/cli/testdata/menu.golden.* from the current output")

// menuFixtureRoot is a small hub over fixed commands, so the golden files pin
// the menu's shape rather than every module's live Short text: two pinned
// modules (one taking a required argument, with a nested group and its
// synthetic self leaf), one remaining module, and a recent leaf.
func menuFixtureRoot() *cobra.Command {
	root := &cobra.Command{Use: "forgectl"}
	noop := func(*cobra.Command, []string) error { return nil }
	leaf := func(use, short string) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, RunE: noop}
	}
	docs := &cobra.Command{Use: "docs", Short: "read project docs"}
	// pin's placeholder is bare uppercase, so only its Args says it needs one.
	pin := &cobra.Command{Use: "pin NAME", Short: "pin a doc", Args: cobra.ExactArgs(1), RunE: noop}
	docs.AddCommand(leaf("list [dir|file ...]", "list the indexed docs"), leaf("read <file>", "read one doc"), pin)
	pr := leaf("pr <ref>", "review a pull request")
	findings := &cobra.Command{Use: "findings", Short: "list or reclaim findings"}
	findings.AddCommand(leaf("list", "list findings directories"))
	pr.AddCommand(leaf("prs", "list open PRs"), findings, leaf("reviewed [<ref>]", "mark a PR reviewed"))
	doctor := leaf("doctor", "check the install")
	for i, m := range []*cobra.Command{docs, pr, doctor} {
		tier := hubTierCore
		if m == doctor {
			tier = hubTierExtension
		}
		stampHubAnnotations(m, i, tier)
		root.AddCommand(m)
	}
	return root
}

func menuFixtureDoc(t *testing.T) (menuJSON, tui.HubHeader) {
	t.Helper()
	root := menuFixtureRoot()
	prs, _, err := root.Find([]string{"pr", "prs"})
	if err != nil {
		t.Fatal(err)
	}
	header := tui.HubHeader{Project: "forgectl", Branch: "main", HasTmux: true, TmuxSessions: 3, HasReviews: true, ReviewsRunning: 1, ReviewsQueued: 2}
	return menuDocument(root, collectHubSections(root, true, []*cobra.Command{prs}), header), header
}

// TestMenu_Golden pins `menu --json` and `menu`'s text form over the fixture
// hub. Regenerate with `go test ./internal/cli -run TestMenu_Golden
// -update-menu-golden` and review the diff.
//
// Mutations that turn it red: stop skipping the synthetic self leaf in
// menuLeaves (pr gains a "pr pr" leaf); count an optional <…> in
// usageRequiresArg (reviewed's needs_args flips); drop the Args clause from
// menuNeedsArgs (pin NAME's needs_args flips); drop the nested menuLeaves
// call (findings loses its list leaf).
func TestMenu_Golden(t *testing.T) {
	doc, header := menuFixtureDoc(t)
	var js bytes.Buffer
	enc := menuEncoder(&js)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	var txt bytes.Buffer
	if err := writeMenuText(&txt, doc, header); err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct {
		file string
		got  []byte
	}{
		{"menu.golden.json", js.Bytes()},
		{"menu.golden.txt", txt.Bytes()},
	} {
		path := filepath.Join("testdata", g.file)
		if *updateMenuGolden {
			if err := os.WriteFile(path, g.got, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Fatalf("read %s (regenerate with -update-menu-golden): %v", path, err)
		}
		if !bytes.Equal(g.got, want) {
			t.Errorf("%s differs from the current output\n got:\n%s\nwant:\n%s", g.file, g.got, want)
		}
	}
}

// TestMenuHeader_NullsEscapesAndCaps pins the header's availability and
// free-text rules: an unavailable field is null (never a zero), an empty
// project drops the branch with it, and the project is escaped and capped.
//
// Mutations that turn it red: drop the HasTmux or HasReviews check in
// menuHeader; copy h.Project without termsafe.SafeLineMax; emit the branch
// when the project is empty.
func TestMenuHeader_NullsEscapesAndCaps(t *testing.T) {
	raw, err := json.Marshal(menuHeader(tui.HubHeader{Branch: "main", TmuxSessions: 4, ReviewsQueued: 9}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"project":null,"branch":null,"tmux_sessions":null,"reviews":null}`; string(raw) != want {
		t.Errorf("unavailable header = %s, want %s", raw, want)
	}

	hostile := "evil\x1b]0;pwned\x07" + strings.Repeat("x", 400)
	h := menuHeader(tui.HubHeader{Project: hostile, Branch: "main"})
	if h.Project == nil || h.Branch == nil {
		t.Fatalf("project/branch dropped: %+v", h)
	}
	if strings.ContainsAny(*h.Project, "\x1b\x07") {
		t.Errorf("project carries a raw control: %q", *h.Project)
	}
	if n := len([]rune(*h.Project)); n > menuTextMaxRunes+len([]rune(termsafe.TruncatedMarker)) {
		t.Errorf("project is %d runes, want at most %d plus the marker", n, menuTextMaxRunes)
	}
}

func TestUsageRequiresArg(t *testing.T) {
	for use, want := range map[string]bool{
		"pr <ref>":                    true,
		"worktree <query> [branch]":   true,
		"list [dir|file ...]":         false,
		"reviewed [<ref>]":            false,
		"docs":                        false,
		"rename <old> <new>":          true,
		"clone [query | url | o/r]":   false,
		"cleanup <YYYY-MM-DD> [--x]":  true,
		"organize [<a> [<b>]] <must>": true,
	} {
		if got := usageRequiresArg(use); got != want {
			t.Errorf("usageRequiresArg(%q) = %v, want %v", use, got, want)
		}
	}
}

// TestMenuJSON_LiveTree runs `menu --json` through fang on the production
// tree with no TTY: exit 0, nothing on stderr, and one document that lists
// every hub module exactly once — the pinned ones first, in hubPinned order —
// with every row's argv resolving to the command whose Short it reports. It
// runs nothing: the only subprocess is the header's read-only tmux listing,
// and shell-history arguments never reach the output.
//
// Mutations that turn it red: drop a section from menuDocument; take a row's
// description from anything but its command's Short; pass the history line
// through to a recent row.
func TestMenuJSON_LiveTree(t *testing.T) {
	isolateJSONContractEnv(t)
	hist := filepath.Join(t.TempDir(), "histfile")
	const marker = "SEKRIT-menu-730" //nolint:gosec // G101: a fake secret the test plants in history
	if err := os.WriteFile(hist, []byte("forgectl pr prs "+marker+"\nforgectl pr prs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HISTFILE", hist)

	runner := &exec.FakeRunner{}
	root := productionJSONRoot(runner)
	stdout, stderr, err := runJSONThroughFang(t, root, "menu", "--json")
	if err != nil {
		t.Fatalf("menu --json: %v (stderr %q)", err, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if strings.Contains(stdout, marker) {
		t.Error("a shell-history argument reached menu --json")
	}
	var doc menuJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"header", "first_run", "pinned", "recent", "commands"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("key %q missing", k)
		}
	}

	var pinned []string
	for _, r := range doc.Pinned {
		pinned = append(pinned, r.Command)
	}
	if !slices.Equal(pinned, hubPinned) {
		t.Errorf("pinned = %v, want %v", pinned, hubPinned)
	}
	if len(doc.Recent) != 1 || doc.Recent[0].Command != "pr prs" {
		t.Errorf("recent = %+v, want the one pr prs row", doc.Recent)
	}
	seen := map[string]int{}
	for _, r := range append(append([]menuRowJSON(nil), doc.Pinned...), doc.Commands...) {
		seen[r.Command]++
	}
	for _, m := range hubModules(root) {
		if seen[m.Name()] != 1 {
			t.Errorf("module %q appears %d times across pinned and commands", m.Name(), seen[m.Name()])
		}
	}

	checked, argsRefused := 0, 0
	var walk func(rows []menuRowJSON)
	walk = func(rows []menuRowJSON) {
		for _, r := range rows {
			checked++
			cmd, _, err := root.Find(r.Argv)
			if err != nil || cmd == root || strings.Join(commandArgv(cmd), " ") != r.Command {
				t.Errorf("row %q: argv %q resolves to %v (err %v)", r.Command, r.Argv, cmd, err)
				continue
			}
			if r.Description != cmd.Short {
				t.Errorf("row %q: description %q, want its Short %q", r.Command, r.Description, cmd.Short)
			}
			if r.NeedsArgs != menuNeedsArgs(cmd) {
				t.Errorf("row %q: needs_args %v for Use %q", r.Command, r.NeedsArgs, cmd.Use)
			}
			// The contract an agent relies on: needs_args false means the
			// bare argv passes the command's own argument check.
			if cmd.Args != nil && cmd.Args(cmd, nil) != nil {
				argsRefused++
				if !r.NeedsArgs {
					t.Errorf("row %q: needs_args false, but its Args refuses the bare argv", r.Command)
				}
			}
			// And the usage line says so, so the hub's parentTakesArg agrees.
			if r.NeedsArgs && !parentTakesArg(cmd) {
				t.Errorf("row %q needs an argument its Use %q does not name", r.Command, cmd.Use)
			}
			if r.Leaves == nil {
				t.Errorf("row %q: leaves is null, want []", r.Command)
			}
			walk(r.Leaves)
		}
	}
	walk(doc.Pinned)
	walk(doc.Recent)
	walk(doc.Commands)
	if checked < 50 {
		t.Errorf("walked only %d rows; the tree walk is broken", checked)
	}
	if argsRefused < 3 {
		t.Errorf("only %d rows refuse a bare argv; env set/get and proxy use alone are 3", argsRefused)
	}

	for _, c := range runner.Calls {
		if c.Name != "tmux" || !slices.Contains(c.Args, "list-sessions") {
			t.Errorf("menu ran %s %q; it may only read tmux sessions", c.Name, c.Args)
		}
	}
}

// TestMenu_TextHasNoEscapes pins ADR-0008 rule 5 for the text form: plain
// lines, no ANSI, and every section heading.
func TestMenu_TextHasNoEscapes(t *testing.T) {
	isolateJSONContractEnv(t)
	t.Setenv("HISTFILE", filepath.Join(t.TempDir(), "absent"))
	stdout, stderr, err := runJSONThroughFang(t, productionJSONRoot(&exec.FakeRunner{}), "menu")
	if err != nil {
		t.Fatalf("menu: %v (stderr %q)", err, stderr)
	}
	if strings.ContainsRune(stdout, 0x1b) {
		t.Error("menu's text form carries an escape sequence")
	}
	for _, want := range []string{"\npinned\n", "\nall commands (", "  forgectl pr <ref>  "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("menu text lacks %q:\n%s", want, stdout)
		}
	}
}
