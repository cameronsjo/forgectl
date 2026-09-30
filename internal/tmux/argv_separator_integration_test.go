//go:build unix

package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTrailingSeparatorOperandsLandVerbatimIsolated is forgectl#823 item 1 on
// a real tmux. A directory ending in ';' and command arguments ending in ';'
// must reach the session and the window exactly as given. Unescaped, tmux's
// argv splitter strips the ';' from the directory (the session starts in the
// sibling directory without it) and ends the new-window command at the first
// argument carrying one, which makes the rest a bogus tmux command.
//
// Every path is compared by suffix, because macOS resolves /tmp to
// /private/tmp in a shell's pwd.
//
// Mutation that turns it red: return s unchanged from escapeArgvSeparator.
func TestTrailingSeparatorOperandsLandVerbatimIsolated(t *testing.T) {
	c, _, _ := isolatedTmux(t)
	ctx := context.Background()

	root, err := os.MkdirTemp("/tmp", "f823-argv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	semi := filepath.Join(root, "wd;")
	// The sibling without the ';' exists too, so an unescaped -c lands in a
	// real directory instead of failing loudly.
	for _, dir := range []string{semi, filepath.Join(root, "wd")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	session, err := c.CreateSession(ctx, "semi", semi)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || !strings.HasSuffix(sessions[0].Path, "/wd;") {
		t.Fatalf("sessions = %+v, want one session whose path ends in /wd;", sessions)
	}

	out := filepath.Join(root, "out")
	script := `{ pwd; printf '%s|' "$@"; } > "$OUT.tmp" && mv "$OUT.tmp" "$OUT"; sleep 60;`
	if _, err := c.NewWindowWithEnv(ctx, session, "w", semi, []string{"OUT=" + out},
		"sh", "-c", script, "sh", "a;", `b\;`, ";", "c"); err != nil {
		t.Fatalf("NewWindowWithEnv: %v", err)
	}
	var got []byte
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err = os.ReadFile(filepath.Clean(out))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the window's command never wrote %s: %v", out, err)
	}
	pwd, args, _ := strings.Cut(string(got), "\n")
	if !strings.HasSuffix(pwd, "/wd;") {
		t.Errorf("window ran in %q, want a directory ending in /wd;", pwd)
	}
	if want := `a;|b\;|;|c|`; args != want {
		t.Errorf("window's command got arguments %q, want %q", args, want)
	}
}

// TestEnvValueEndingInSemicolonLandsIsolated is forgectl#836 item 4 on a real
// tmux: an -e value ending in ';' reaches the window's environment as given,
// and the command after it still runs.
//
// Mutation that turns it red: append e rather than escapeArgvSeparator(e) at
// NewWindowWithEnv's -e (tmux ends the command at the entry and fails).
func TestEnvValueEndingInSemicolonLandsIsolated(t *testing.T) {
	c, _, _ := isolatedTmux(t)
	ctx := context.Background()

	root, err := os.MkdirTemp("/tmp", "f836-env-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	session, err := c.CreateSession(ctx, "env", root)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	out := filepath.Join(root, "out")
	script := `printf '%s|%s' "$SEMI" "$HASH" > "$OUT.tmp" && mv "$OUT.tmp" "$OUT"; sleep 60`
	if _, err := c.NewWindowWithEnv(ctx, session, "w", root,
		[]string{"OUT=" + out, "SEMI=a;", "HASH=#{pid}"}, "sh", "-c", script); err != nil {
		t.Fatalf("NewWindowWithEnv: %v", err)
	}
	var got []byte
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err = os.ReadFile(filepath.Clean(out))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the window's command never wrote %s: %v", out, err)
	}
	if want := "a;|#{pid}"; string(got) != want {
		t.Errorf("window environment = %q, want %q", got, want)
	}
}
