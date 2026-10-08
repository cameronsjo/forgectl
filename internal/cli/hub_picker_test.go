package cli

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// pickerCommands walks the live tree for every command the hub picker can
// open on: any command whose Use names one argument tui.PickerSpec accepts.
// That is a superset of today's picker rows (module rows, leaves, recent
// rows), so a row added later is already covered.
func pickerCommands(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if _, _, ok := tui.PickerSpec(sub.Use); ok {
				out = append(out, sub)
			}
			walk(sub)
		}
	}
	walk(root)
	return out
}

// childTokens is every name and alias a value could use to reach one of
// cmd's children, hidden ones included; for launch, also every token the
// launch intercept hands to launch's own verbs.
func childTokens(root, cmd *cobra.Command) []string {
	var toks []string
	for _, c := range cmd.Commands() {
		toks = append(toks, c.Name())
		toks = append(toks, c.Aliases...)
	}
	if cmd == findChild(root, "launch") {
		for verb := range ownLaunchVerbs {
			toks = append(toks, verb)
		}
		for _, aliases := range launchAliases {
			toks = append(toks, aliases...)
		}
	}
	return toks
}

// TestHubPickerArgv_RefusesEveryChildOfEveryPickerCommand is the review's
// Important 1, derived from the tree rather than a name list: for every
// command the picker can open on, a value naming any of its children — or
// any of launch's own verbs — is refused, so it can never dispatch to that
// child. `pr drain`, `pr repair`, `pr local`, and the resume children are
// all in here, and a subcommand added tomorrow will be too.
func TestHubPickerArgv_RefusesEveryChildOfEveryPickerCommand(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	build := hubPickerArgv(root)
	cmds := pickerCommands(root)
	if len(cmds) < 10 {
		t.Fatalf("found only %d picker commands; the walk is broken", len(cmds))
	}
	checked := 0
	for _, cmd := range cmds {
		prefix := commandArgv(cmd)
		_, optional, _ := tui.PickerSpec(cmd.Use)
		for _, tok := range childTokens(root, cmd) {
			if tok == "" || strings.HasPrefix(tok, "-") {
				continue
			}
			checked++
			argv, err := build(prefix, tok, optional)
			if !errors.Is(err, errPickerSubcommand) {
				t.Errorf("picker for %q accepted %q as its argument: argv %q, err %v", strings.Join(prefix, " "), tok, argv, err)
			}
		}
		// Control: an ordinary value is accepted and still dispatches to cmd.
		argv, err := build(prefix, "zz-plain-value", optional)
		if err != nil {
			t.Errorf("picker for %q refused a plain value: %v", strings.Join(prefix, " "), err)
			continue
		}
		if got := hubDispatchTarget(root, argv); got != cmd {
			t.Errorf("picker argv %q for %q dispatches elsewhere", argv, strings.Join(prefix, " "))
		}
	}
	for _, must := range [][]string{{"pr", "drain"}, {"pr", "repair"}, {"pr", "local"}, {"resume", "outdated"}, {"launch", "stats"}} {
		if _, err := build(must[:1], must[1], true); !errors.Is(err, errPickerSubcommand) {
			t.Errorf("picker for %q accepted %q (err %v)", must[0], must[1], err)
		}
	}
	if checked < 20 {
		t.Fatalf("checked only %d child tokens; the enumeration is broken", checked)
	}
}

// TestHubPickerArgv_DashTerminatorParsesUnchanged pins the defense in depth:
// where "--" is inserted it changes nothing about how the command parses —
// pflag drops it and the value arrives as the only positional — and it is
// not inserted where it would be read (launch's harness args, a command that
// parses no flags, docker's `[-- args...]`).
func TestHubPickerArgv_DashTerminatorParsesUnchanged(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	build := hubPickerArgv(root)
	dashed := 0
	for _, cmd := range pickerCommands(root) {
		prefix := commandArgv(cmd)
		_, optional, _ := tui.PickerSpec(cmd.Use)
		argv, err := build(prefix, "zz-value", optional)
		if err != nil {
			t.Fatalf("%q: %v", strings.Join(prefix, " "), err)
		}
		hasDash := slices.Contains(argv, "--")
		if hasDash != dashTerminatorSafe(root, cmd) {
			t.Errorf("%q: argv %q, dash-safe=%v", strings.Join(prefix, " "), argv, dashTerminatorSafe(root, cmd))
		}
		if !hasDash {
			continue
		}
		dashed++
		fresh := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
		target, rest, err := fresh.Find(argv)
		if err != nil {
			t.Fatalf("%q: Find: %v", argv, err)
		}
		if err := target.ParseFlags(rest); err != nil {
			t.Fatalf("%q: ParseFlags: %v", argv, err)
		}
		if got := target.Flags().Args(); !slices.Equal(got, []string{"zz-value"}) {
			t.Errorf("%q parses to positionals %q, want exactly [zz-value]", argv, got)
		}
	}
	if dashed == 0 {
		t.Fatal("no picker command took the terminator; the check proves nothing")
	}
	for _, prefix := range [][]string{{"launch"}, {"docker", "build"}} {
		argv, err := build(prefix, "zz-value", true)
		if err != nil || slices.Contains(argv, "--") {
			t.Errorf("%q: argv %q (err %v), want no inserted terminator", prefix, argv, err)
		}
	}
}

// TestHubRunOptions_WiresTheTreeAwareBuilder pins that the hub the operator
// actually opens uses hubPickerArgv, not the tree-blind default.
func TestHubRunOptions_WiresTheTreeAwareBuilder(t *testing.T) {
	t.Setenv("HISTFILE", t.TempDir()+"/absent-history")
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	opts := hubRunOptions(t.Context(), module.Deps{Runner: &exec.FakeRunner{}}, root, nil)
	if opts.BuildArgv == nil {
		t.Fatal("hubRunOptions left BuildArgv nil; the picker would fall back to tree-blind argv")
	}
	if _, err := opts.BuildArgv([]string{"pr"}, "drain", false); !errors.Is(err, errPickerSubcommand) {
		t.Errorf("the wired builder accepted `pr drain` (err %v)", err)
	}
}

// TestHubPicker_NoPickerRowFeedsTheHarness pins the review's I3 by shape, not
// by name: no command the picker can open on is dispatched through the launch
// intercept to the harness launcher, where the launcher — not forgectl's tree —
// decides what a first word means (`update` → `claude update`). Today that is
// `launch [harness args…]`, kept off the picker by the variadic rule; a new
// harness-bound row would fail here.
func TestHubPicker_NoPickerRowFeedsTheHarness(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	launchCmd := findChild(root, "launch")
	if launchCmd == nil {
		t.Fatal("no launch command; the check proves nothing")
	}
	if _, _, ok := tui.PickerSpec(launchCmd.Use); ok {
		t.Errorf("launch (%q) opens the picker; its argument is a harness argv", launchCmd.Use)
	}
	for _, cmd := range pickerCommands(root) {
		if hubDispatchRoute(root, commandArgv(cmd)).launcher {
			t.Errorf("picker row %q is dispatched to the harness launcher", strings.Join(commandArgv(cmd), " "))
		}
	}
	for _, e := range buildHub(root, true, []*cobra.Command{launchCmd}) {
		if e.Name == "launch" && e.Argv != nil && e.NeedsArgs {
			if _, _, ok := tui.PickerSpec(e.Use); ok {
				t.Errorf("recent launch row would open the picker: %+v", e)
			}
		}
	}
}

// optOutRoot is a minimal tree for the hub-no-picker annotation: wrap's own
// positional and its pass subverb both name another CLI's subcommand and
// carry the annotation; plain is the same shape without it.
func optOutRoot() *cobra.Command {
	root := &cobra.Command{Use: "forgectl"}
	noop := func(*cobra.Command, []string) error { return nil }
	wrap := &cobra.Command{
		Use:         "wrap <sub>",
		Short:       "wrap another CLI",
		Args:        cobra.MaximumNArgs(1),
		RunE:        noop,
		Annotations: map[string]string{hubNoPickerAnnotation: "the argument is the wrapped CLI's subcommand"},
	}
	wrap.AddCommand(
		&cobra.Command{
			Use: "pass <sub>", Short: "pass one subcommand through", Args: cobra.ExactArgs(1), RunE: noop,
			Annotations: map[string]string{hubNoPickerAnnotation: "the argument is the wrapped CLI's subcommand"},
		},
		&cobra.Command{Use: "plain <name>", Short: "take a plain value", Args: cobra.ExactArgs(1), RunE: noop},
	)
	root.AddCommand(wrap)
	return root
}

// TestHubPicker_NoPickerAnnotationIsRefused pins the hub-no-picker opt-out
// (forgectl#730): a command carrying the annotation is refused by the
// tree-aware argv builder, and every hub row built for it is marked NoPicker
// so the picker never opens; the unannotated sibling is unaffected.
//
// Mutations that turn it red: drop the hasNoPickerAnnotation check from
// hubPickerArgv (the builder accepts wrap and pass), or move it below the
// empty-value return (an empty value runs them bare); set NoPicker false in
// moduleEntry, buildLeaves (self or sub leaf) or recentEntry.
func TestHubPicker_NoPickerAnnotationIsRefused(t *testing.T) {
	root := optOutRoot()
	build := hubPickerArgv(root)
	for _, prefix := range [][]string{{"wrap"}, {"wrap", "pass"}} {
		if argv, err := build(prefix, "zz-value", false); !errors.Is(err, errPickerOptedOut) {
			t.Errorf("builder for %q: argv %q err %v, want errPickerOptedOut", prefix, argv, err)
		}
		// An empty optional value is refused too: it would run the
		// command bare from a picker that should never have opened.
		if argv, err := build(prefix, "", true); !errors.Is(err, errPickerOptedOut) {
			t.Errorf("builder for %q with an empty value: argv %q err %v, want errPickerOptedOut", prefix, argv, err)
		}
	}
	argv, err := build([]string{"wrap", "plain"}, "zz-value", false)
	if err != nil || !slices.Equal(argv, []string{"wrap", "plain", "--", "zz-value"}) {
		t.Errorf("builder for the unannotated sibling: argv %q err %v", argv, err)
	}

	wrap := findChild(root, "wrap")
	entry := moduleEntry(wrap)
	if !entry.NoPicker {
		t.Error("the annotated module row is not marked NoPicker")
	}
	got := map[string]bool{}
	for _, l := range entry.Leaves {
		got[l.Name] = l.NoPicker
	}
	if want := map[string]bool{"wrap": true, "pass": true, "plain": false}; !maps.Equal(got, want) {
		t.Errorf("leaf NoPicker = %v, want %v", got, want)
	}
	if !recentEntry(findChild(wrap, "pass")).NoPicker {
		t.Error("the annotated recent row is not marked NoPicker")
	}
	if recentEntry(findChild(wrap, "plain")).NoPicker {
		t.Error("the unannotated recent row is marked NoPicker")
	}
}

// TestStampHubAnnotations_KeepsTheConstructorsOwn pins that newRoot's hub
// annotations merge into a module's own rather than replacing them, so a
// module root can carry hub-no-picker. Mutation that turns it red: assign a
// fresh map in stampHubAnnotations.
func TestStampHubAnnotations_KeepsTheConstructorsOwn(t *testing.T) {
	cmd := &cobra.Command{Use: "wrap <sub>", Annotations: map[string]string{hubNoPickerAnnotation: "reason"}}
	stampHubAnnotations(cmd, 4, hubTierCore)
	want := map[string]string{hubNoPickerAnnotation: "reason", hubOrderAnnotation: "4", hubTierAnnotation: hubTierCore}
	if !maps.Equal(cmd.Annotations, want) {
		t.Errorf("annotations = %v, want %v", cmd.Annotations, want)
	}
	bare := &cobra.Command{Use: "bare"}
	stampHubAnnotations(bare, 0, hubTierExtension)
	if bare.Annotations[hubTierAnnotation] != hubTierExtension {
		t.Errorf("a command with no annotations was not stamped: %v", bare.Annotations)
	}
}
