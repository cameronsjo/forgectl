//go:build unix

package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// replacedServerRunner passes every command to a real tmux, but rewrites the
// server start time in listing output. The client then revalidates a doctored
// identity successfully, and the guarded action that follows reaches a server
// whose generation does not match. That is exactly the state a server
// replaced between revalidation and action produces, so the action must
// refuse on its own.
type replacedServerRunner struct {
	internalexec.Runner
	pid, start string
}

func (r replacedServerRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := r.Runner.Run(ctx, name, args...)
	if len(args) > 0 && strings.HasPrefix(args[0], "list-") {
		// tmux 3.4 renders FieldSep in -F output as the escaped text \037
		// (escapedFieldSep, format.go), so both spellings are rewritten.
		for _, sep := range []string{FieldSep, escapedFieldSep} {
			actual := r.pid + sep + r.start + sep
			out = strings.ReplaceAll(out, actual, r.pid+sep+r.start+"0"+sep)
		}
	}
	return out, err
}

func sessionNames(t *testing.T, c *Client) []string {
	t.Helper()
	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var names []string
	for _, s := range sessions {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	return names
}

// TestSessionVerbsGenerationGuardIsolated measures the forgectl#785 guard
// against a real tmux 3.4 server. Each session verb and AttachWindow's
// select-window must refuse ON ITS OWN when the server receiving it is not
// the captured incarnation, even after revalidation passed. Under the
// captured generation each verb must still act, and a hostile rename must
// land byte for byte.
//
// Mutation that turns it red: have KillOthers (or KillSession, or
// RenameSession) issue its bare command again; the replaced-server case then
// kills or renames.
func TestSessionVerbsGenerationGuardIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", name, "sleep 60"); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	bravo, err := c.ResolveSessionExact(ctx, "bravo")
	if err != nil {
		t.Fatalf("ResolveSessionExact: %v", err)
	}
	windows, err := c.ListWindows(ctx)
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	var bravoWindow WindowIdentity
	for _, w := range windows {
		if w.SessionID == bravo.ID {
			bravoWindow = c.WindowIdentity(w)
		}
	}
	if bravoWindow.ID == "" {
		t.Fatal("bravo has no window")
	}

	replaced := New(replacedServerRunner{Runner: runner, pid: bravo.Generation.PID, start: bravo.Generation.StartTime},
		WithBins(tmuxBin, "sesh"), WithInsideTmux(func() bool { return true }))
	stranger := bravo
	stranger.Generation.StartTime += "0"
	strangerWindow := bravoWindow
	strangerWindow.Generation.StartTime += "0"

	all := []string{"alpha", "bravo", "charlie"}
	for verb, run := range map[string]func() error{
		"KillOthers":    func() error { return replaced.KillOthers(ctx, stranger) },
		"KillSession":   func() error { return replaced.KillSession(ctx, stranger) },
		"RenameSession": func() error { return replaced.RenameSession(ctx, stranger, "renamed") },
		"AttachWindow":  func() error { return replaced.AttachWindow(ctx, strangerWindow) },
	} {
		if err := run(); !errors.Is(err, ErrGenerationChanged) {
			t.Fatalf("%s against a replaced server: err = %v, want ErrGenerationChanged", verb, err)
		}
		if got := sessionNames(t, c); !slices.Equal(got, all) {
			t.Fatalf("%s against a replaced server changed the sessions to %v", verb, got)
		}
	}

	// Under the captured generation the rename acts, and the quoted name lands
	// exactly as a bare `rename-session -- <name>` lands it. tmux renders
	// some bytes (a '$' or '\') escaped in its own listing, so the reference
	// is a bare rename of alpha to the same name plus a suffix. The name
	// tries to close the quote and run kill-server. It also carries $VAR, ~
	// and backslash, which tmux's parser expands outside single quotes.
	const hostile = `'; kill-server; ' $HOME ~ \x "q"`
	if _, err := runner.Run(ctx, tmuxBin, "rename-session", "-t", "alpha", "--", hostile+" bare"); err != nil {
		t.Fatalf("bare reference rename: %v", err)
	}
	if err := c.RenameSession(ctx, bravo, hostile); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	got := sessionNames(t, c)
	var guardedName string
	for _, name := range got {
		if name != "charlie" && !strings.HasSuffix(name, " bare") {
			guardedName = name
		}
	}
	if len(got) != 3 || !slices.Contains(got, guardedName+" bare") {
		t.Fatalf("after the hostile rename sessions = %q; the guarded rename %q differs from the bare one", got, guardedName)
	}
	if err := c.KillOthers(ctx, bravo); err != nil {
		t.Fatalf("KillOthers: %v", err)
	}
	if got := sessionNames(t, c); !slices.Equal(got, []string{guardedName}) {
		t.Fatalf("after KillOthers sessions = %q, want only %q", got, guardedName)
	}
	if err := c.KillSession(ctx, bravo); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	// A guarded kill of a session that no longer exists fails without being
	// read as success: if-shell cannot find its target and runs nothing.
	if err := c.KillSession(ctx, bravo); err == nil {
		t.Fatal("KillSession of a gone session succeeded")
	}
}
