package cli

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// sessionsRunner serves a list-sessions fixture built from the given names, so
// the command's exact-name resolution and the kill's revalidation both run
// against a server that really holds them. Session ids are assigned in order.
func sessionsRunner(names ...string) *exec.FakeRunner {
	rows := make([]string, 0, len(names))
	for i, name := range names {
		rows = append(rows, strings.Join([]string{
			"123", "456", "$" + strconv.Itoa(i), name, "1", "0", "1700000000", "/w",
		}, "\x1f"))
	}
	out := strings.Join(rows, "\n")
	return &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return out, nil
		}
		return "", nil
	}}
}

// existsRunner holds exactly the session the tests act on.
func existsRunner() *exec.FakeRunner { return sessionsRunner("mysession") }

// absentRunner is a running server with no sessions at all — the "not there"
// case, which is distinct from a server that could not be read.
func absentRunner() *exec.FakeRunner { return sessionsRunner() }

func TestKillCmd_YesFlagSkipsConfirm(t *testing.T) {
	fake := existsRunner()
	client := tmux.New(fake)
	cmd := newTmuxKillCmd(client)

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--yes", "mysession"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// kill-session must have been called, and against the native id.
	found := false
	for _, c := range fake.Calls {
		if len(c.Args) > 0 && c.Args[0] == "kill-session" {
			found = true
			if got := c.Args[len(c.Args)-1]; got != "$0" {
				t.Errorf("kill-session target = %q, want the native id $0", got)
			}
		}
	}
	if !found {
		t.Errorf("kill-session never called; calls: %v", fake.Calls)
	}
	if !strings.Contains(out.String(), "killed mysession") {
		t.Errorf("output missing confirmation: %q", out.String())
	}
}

func TestKillCmd_OthersFlagRoutesToKillOthers(t *testing.T) {
	fake := existsRunner()
	client := tmux.New(fake)
	cmd := newTmuxKillCmd(client)

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--yes", "--others", "mysession"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "except mysession") {
		t.Errorf("output missing --others message: %q", out.String())
	}
}

func TestKillCmd_MissingSessionErrors(t *testing.T) {
	client := tmux.New(absentRunner())
	cmd := newTmuxKillCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"--yes", "nosuch"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("error message = %q, want it to mention no such session", err.Error())
	}
}

// TestKillCmd_PrefixSiblingIsNotKilled is forgectl#237 at the CLI boundary,
// stated as the reported reproduction: `forgectl tmux kill forge` with only
// `forge-review` present must kill NOTHING. Under tmux's own -t resolution the
// bare name fell through to the prefix sibling and killed it.
func TestKillCmd_PrefixSiblingIsNotKilled(t *testing.T) {
	fake := sessionsRunner("forge-review")
	cmd := newTmuxKillCmd(tmux.New(fake))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"--yes", "forge"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("killing an absent session succeeded; a prefix sibling answered for it")
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("error = %q, want it to report no such session", err.Error())
	}
	for _, c := range fake.Calls {
		if len(c.Args) > 0 && c.Args[0] == "kill-session" {
			t.Fatalf("kill-session ran with %v; nothing should have been killed", c.Args)
		}
	}
}

// TestKillCmd_OthersRefusesStaleIdentity guards the highest-blast-radius path:
// `kill --others` kills everything it is NOT pointed at, so a target that has
// gone stale between resolution and dispatch must abort rather than proceed.
func TestKillCmd_OthersRefusesStaleIdentity(t *testing.T) {
	calls := 0
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) == 0 || args[0] != "list-sessions" {
			return "", nil
		}
		calls++
		if calls == 1 {
			// Resolution sees the session.
			return strings.Join([]string{"123", "456", "$0", "mysession", "1", "0", "1700000000", "/w"}, "\x1f"), nil
		}
		// By revalidation time the server has restarted: same $0, new generation.
		return strings.Join([]string{"999", "999", "$0", "mysession", "1", "0", "1700000000", "/w"}, "\x1f"), nil
	}}
	cmd := newTmuxKillCmd(tmux.New(fake))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"--yes", "--others", "mysession"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("kill --others proceeded against a restarted server")
	}
	for _, c := range fake.Calls {
		if len(c.Args) > 0 && c.Args[0] == "kill-session" {
			t.Fatalf("kill-session ran with %v after the generation changed", c.Args)
		}
	}
}
