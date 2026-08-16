package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// hostileTmuxRunner serves listings whose every operator-visible string carries
// terminal controls: session names and paths, window names and their parent
// session name, pane titles and commands, and the sesh candidate list.
func hostileTmuxRunner() *exec.FakeRunner {
	const sep = "\x1f"
	h := termsafetest.Hostile
	session := strings.Join([]string{
		"123", "456", "$0", h("work"), "1", "1", "1700000000", h("/tmp/w"),
	}, sep)
	window := strings.Join([]string{
		"123", "456", "@0", "$0", h("work"), "0", h("edit"), "1", "2",
	}, sep)
	panes := strings.Join([]string{
		strings.Join([]string{"123", "456", "%0", "@0", "0", h("t"), h("vim"), "1"}, sep),
		// Empty command: the tree falls back to the pane title, a second sink.
		strings.Join([]string{"123", "456", "%1", "@0", "1", h("logs"), "", "0"}, sep),
	}, "\n")

	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "sesh") {
			return h("candidate") + "\n" + h("other"), nil
		}
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return session, nil
		case "list-windows":
			return window, nil
		case "list-panes":
			return panes, nil
		}
		return "", nil
	}}
}

func hostileTmuxClient() *tmux.Client {
	return tmux.New(hostileTmuxRunner(),
		tmux.WithLookPath(func(string) (string, error) { return "/usr/bin/sesh", nil }))
}

// TestTmuxListingsEmitNothingUnsafe covers every tmux read verb that prints a
// tmux-derived string. A session, window, or pane name is chosen by whoever
// created the object — any same-uid process — so each of these is a terminal
// boundary over untrusted text.
func TestTmuxListingsEmitNothingUnsafe(t *testing.T) {
	client := hostileTmuxClient()
	for _, tc := range []struct {
		verb string
		cmd  func() *cobra.Command
	}{
		{"tmux ls", func() *cobra.Command { return newTmuxLsCmd(client) }},
		{"tmux windows", func() *cobra.Command { return newTmuxWindowsCmd(client) }},
		{"tmux tree", func() *cobra.Command { return newTmuxTreeCmd(client) }},
		{"tmux pick", func() *cobra.Command { return newTmuxPickCmd(client) }},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			var out bytes.Buffer
			cmd := tc.cmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(nil)
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.verb, err)
			}
			if out.Len() == 0 {
				t.Fatalf("%s produced no output; the check would pass vacuously", tc.verb)
			}
			termsafetest.AssertInert(t, tc.verb, out.String())
		})
	}
}

// TestTmuxListingsKeepOrdinaryValuesVerbatim is the other half: neutralizing
// must be invisible on values that were never dangerous, so the tables an
// operator reads every day are byte-for-byte what they were.
func TestTmuxListingsKeepOrdinaryValuesVerbatim(t *testing.T) {
	const sep = "\x1f"
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return strings.Join([]string{"1", "2", "$0", "forge", "3", "1", "1700000000", "/repo/forge"}, sep), nil
		}
		return "", nil
	}}
	var out bytes.Buffer
	cmd := newTmuxLsCmd(tmux.New(fake))
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := out.String(), "●  forge  3 windows  /repo/forge\n"; got != want {
		t.Errorf("ordinary listing changed:\ngot  %q\nwant %q", got, want)
	}
}
