//go:build unix

package tmux

import (
	"context"
	"os"
	"testing"
)

// TestUTF8SessionNameRoundTripsUnderCLocale is forgectl#840 against a real
// tmux: under LANG=C with $TMUX unset, a session created as "café" must list,
// resolve and ensure as "café".
//
// Measured on tmux 3.4 without `-u`: the server stores the name intact, but a
// command client in a non-UTF-8 locale runs its output through utf8_sanitize,
// so every listing renders it "caf_". The exact lookup then misses the
// session forgectl just created, and EnsureSession's create fails with
// "duplicate session: caf_". tmux 3.7b/3.7c take the same sanitize path and
// also turn the 0x1F field separator into "_". With `-u` the client is
// CLIENT_UTF8 and neither happens, so every assertion below holds on both.
//
// $TMUX is unset, not emptied: any value, even "", puts tmux in UTF-8 mode and
// would make this test pass without the flag.
//
// Mutation that turns it red: drop "-u" from tmuxArgs.
func TestUTF8SessionNameRoundTripsUnderCLocale(t *testing.T) {
	c, _, _ := isolatedTmux(t)
	unsetEnv(t, "TMUX")
	unsetEnv(t, "TMUX_PANE")
	unsetEnv(t, "LC_CTYPE")
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	ctx := context.Background()
	const name = "café"

	created, err := c.CreateSession(ctx, name, "")
	if err != nil {
		t.Fatalf("CreateSession(%q): %v", name, err)
	}

	sessions, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s.ID == created.ID {
			found = true
			if s.Name != name {
				t.Errorf("ListSessions names %s %q, want %q", s.ID, s.Name, name)
			}
		}
	}
	if !found {
		t.Fatalf("ListSessions = %+v, want the created session %s", sessions, created.ID)
	}

	resolved, err := c.ResolveSessionExact(ctx, name)
	if err != nil {
		t.Fatalf("ResolveSessionExact(%q): %v", name, err)
	}
	if resolved.ID != created.ID {
		t.Errorf("ResolveSessionExact = %s, want %s", resolved.ID, created.ID)
	}

	ensured, err := c.EnsureSession(ctx, name, "")
	if err != nil {
		t.Fatalf("EnsureSession(%q) over the existing session: %v", name, err)
	}
	if ensured.ID != created.ID {
		t.Errorf("EnsureSession = %s, want the existing %s (a second session was made)", ensured.ID, created.ID)
	}
}

// unsetEnv removes key for the rest of the test and restores it afterwards.
// t.Setenv registers the restore; the Unsetenv then removes the value it set.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}
