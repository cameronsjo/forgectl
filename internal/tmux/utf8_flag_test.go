package tmux

import (
	"context"
	"slices"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// TestArgvBuildersPlaceUTF8Flag pins the raw argv shape of forgectl#840, which
// FakeRunner hides from every other test (it presents tmux argv without the
// `-u`). A non-interactive argv carries `-u`, after the `-S <path>` pin so the
// pin stays the two leading elements pinnedArgs matches; the attach-path
// builder carries no `-u` at all.
//
// Mutations that turn it red: drop "-u" from tmuxArgs; put it ahead of the
// pin; route interactiveArgs through tmuxArgs.
func TestArgvBuildersPlaceUTF8Flag(t *testing.T) {
	unpinned := New(&internalexec.FakeRunner{})
	pinned := pinnedClient(t, &internalexec.FakeRunner{})
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{"unpinned command", unpinned.tmuxArgs("list-sessions"), []string{"-u", "list-sessions"}},
		{"pinned command", pinned.tmuxArgs("list-sessions"), []string{"-S", testSocket, "-u", "list-sessions"}},
		{"unpinned attach", unpinned.interactiveArgs("attach-session", "-t", "$1"), []string{"attach-session", "-t", "$1"}},
		{"pinned attach", pinned.interactiveArgs("attach-session", "-t", "$1"), []string{"-S", testSocket, "attach-session", "-t", "$1"}},
	}
	for _, tt := range tests {
		if !slices.Equal(tt.got, tt.want) {
			t.Errorf("%s: argv = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	if !pinned.pinnedArgs(pinned.tmuxArgs("list-sessions")) {
		t.Error("pinnedArgs refuses the argv tmuxArgs builds: the -u sits between the pin's two elements")
	}
}

// TestAttachPathOmitsUTF8Flag drives the attach path both outside tmux
// (attach-session, the interactive Runner path) and inside it (switch-client),
// and asserts that neither carries `-u` while the revalidation listing that
// precedes each one does (forgectl#840). `-u` is for command clients whose -F
// output forgectl parses; on attach it would override tmux's reading of what
// the operator's terminal can render.
//
// Mutations that turn it red: build any attach-path argv with tmuxArgs instead
// of interactiveArgs (the jump gains -u); build the revalidation list with
// interactiveArgs (the listing loses it).
func TestAttachPathOmitsUTF8Flag(t *testing.T) {
	const pid, start = "9", "1"
	sessionRow := strings.Join([]string{pid, start, "$1", "forge", "1", "0", "0", "/tmp"}, FieldSep)
	windowRow := strings.Join([]string{pid, start, "@1", "$1", "forge", "0", "editor", "1", "1"}, FieldSep)
	lastRow := strings.Join([]string{pid, start, "5", "$1", "forge"}, FieldSep)

	for _, inside := range []bool{false, true} {
		run := &internalexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
			switch tmuxVerb(args) {
			case "list-sessions":
				if slices.Contains(args, lastAttachedFormat) {
					return lastRow, nil
				}
				return sessionRow, nil
			case "list-windows":
				return windowRow, nil
			}
			return "", nil
		}}
		c := New(run, WithInsideTmux(func() bool { return inside }))
		c.getenv = func(string) string { return "" }
		ctx := context.Background()
		gen := ServerGeneration{Selector: c.currentSelector(), PID: pid, StartTime: start}

		if err := c.AttachSession(ctx, SessionIdentity{Generation: gen, ID: "$1", Name: "forge"}); err != nil {
			t.Fatalf("inside=%v AttachSession: %v", inside, err)
		}
		if err := c.AttachWindow(ctx, WindowIdentity{Generation: gen, ID: "@1", SessionID: "$1", Name: "editor"}); err != nil {
			t.Fatalf("inside=%v AttachWindow: %v", inside, err)
		}
		if err := c.LastSession(ctx); err != nil {
			t.Fatalf("inside=%v LastSession: %v", inside, err)
		}

		jumps := 0
		for _, call := range run.Calls {
			switch verb := tmuxVerb(call.Args); verb {
			case "attach-session", "switch-client":
				jumps++
				if call.TmuxUTF8 {
					t.Errorf("inside=%v: %s carries -u: %q", inside, verb, call.Args)
				}
				if call.Interactive != (verb == "attach-session") {
					t.Errorf("inside=%v: %s went through the wrong Runner path (interactive=%v)", inside, verb, call.Interactive)
				}
			default:
				if !call.TmuxUTF8 {
					t.Errorf("inside=%v: non-interactive %s lacks -u: %q", inside, verb, call.Args)
				}
			}
		}
		// Three jumps: one per verb driven. Inside tmux, LastSession's
		// switch-client -l is one of them; outside, its attach-session is.
		if jumps != 3 {
			t.Errorf("inside=%v: %d attach/switch calls recorded, want 3 (calls: %+v)", inside, jumps, run.Calls)
		}
	}
}
