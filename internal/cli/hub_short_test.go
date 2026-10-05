package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// shortProblem reports why cmd's Short is not a real one-line description
// (forgectl#730 item 3), or "" when it is. The hub shows Short beside every
// row, so a missing one renders a bare name, and one that just repeats the
// name or usage line tells the operator nothing the row did not.
func shortProblem(cmd *cobra.Command) string {
	short := strings.TrimSpace(cmd.Short)
	switch {
	case short == "":
		return "has no Short"
	case short == cmd.Name(), short == strings.TrimSpace(cmd.Use):
		return "Short " + short + " only repeats the name or usage"
	case strings.Contains(short, "\n"):
		return "Short spans more than one line"
	}
	return ""
}

// assertRealShorts walks every available command under root.
func assertRealShorts(t *testing.T, label string, root *cobra.Command) {
	t.Helper()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !sub.IsAvailableCommand() {
				continue
			}
			if p := shortProblem(sub); p != "" {
				t.Errorf("[%s] %s %s", label, sub.CommandPath(), p)
			}
			walk(sub)
		}
	}
	walk(root)
}

// TestEveryHubLeafHasARealShort covers the live tree and the config-error
// trees projects and review swap in under a broken config — the hub lists
// those too, and their stubs were where rows went blank.
func TestEveryHubLeafHasARealShort(t *testing.T) {
	assertRealShorts(t, "live tree", newRoot(module.Deps{Runner: &exec.FakeRunner{}}))

	cause := errors.New("categorical")
	for _, stub := range []*cobra.Command{newProjectsConfigErrorCmd(cause), newReviewConfigErrorCmd(cause)} {
		wrapper := &cobra.Command{Use: "forgectl"}
		wrapper.AddCommand(stub)
		assertRealShorts(t, "config-error tree", wrapper)
	}
}

// TestShortProblem is the check's own control: each bad shape it exists to
// catch is caught.
func TestShortProblem(t *testing.T) {
	for _, tc := range []struct {
		cmd  *cobra.Command
		want bool
	}{
		{&cobra.Command{Use: "list [query]"}, true},
		{&cobra.Command{Use: "list [query]", Short: "list [query]"}, true},
		{&cobra.Command{Use: "list [query]", Short: "list"}, true},
		{&cobra.Command{Use: "list", Short: "  "}, true},
		{&cobra.Command{Use: "list", Short: "one\ntwo"}, true},
		{&cobra.Command{Use: "list [query]", Short: "List projects"}, false},
	} {
		if got := shortProblem(tc.cmd) != ""; got != tc.want {
			t.Errorf("shortProblem(Use=%q Short=%q) flagged=%v, want %v", tc.cmd.Use, tc.cmd.Short, got, tc.want)
		}
	}
}
