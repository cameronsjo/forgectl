//go:build unix

package tmuxadapter

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// createCWD returns the operand that follows -c in the recorded create argv.
func createCWD(t *testing.T, calls []exec.SensitiveCommand) exec.Arg {
	t.Helper()
	for _, cmd := range calls {
		if cmd.Kind != exec.KindTmuxCreate {
			continue
		}
		for i := 0; i+1 < len(cmd.Args); i++ {
			if cmd.Args[i].Equal(exec.MustFixed("-c")) {
				return cmd.Args[i+1]
			}
		}
		t.Fatal("the create argv carries no -c")
	}
	t.Fatal("no create command was recorded")
	return exec.Arg{}
}

// TestStartEscapesTheStartDirectory pins the create argv's -c operand
// (forgectl#839, forgectl#836 item 2). tmux format-expands it and splits a
// command at an element ending in ';', so a directory holding either reaches
// tmux re-spelled, and an ordinary directory reaches it byte for byte.
//
// Mutation that turns it red: pass spec.CWD() unmapped at the create's -c.
func TestStartEscapesTheStartDirectory(t *testing.T) {
	for _, tc := range []struct{ cwd, want string }{
		{testCWD, testCWD},
		{"/w/x#(touch m)", "/w/x##(touch m)"},
		{"/w/#{session_name}", "/w/##{session_name}"},
		{"/w/semi;", `/w/semi\;`},
		{"/w/both#;", `/w/both##\;`},
	} {
		tag, err := backend.NewRecoveryTag()
		if err != nil {
			t.Fatal(err)
		}
		spec, err := backend.NewStartSpec(tc.cwd, "forgectl", tag, bootstrapForTest(t))
		if err != nil {
			t.Fatalf("NewStartSpec(%q): %v", tc.cwd, err)
		}
		name := spec.OwnershipName()
		run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
			exec.KindTmuxCreate: func() (exec.SensitiveResult, error) {
				return stdout(row("91", "1700000000", "$1", name)), nil
			},
		}}.runFunc}
		if got := newTestAdapter(t, run, nil).Start(context.Background(), spec).Outcome(); got != backend.RefKnown {
			t.Fatalf("Start(%q) outcome = %v, want ref-known", tc.cwd, got)
		}
		if !createCWD(t, run.Calls()).Equal(exec.Opaque(tc.want)) {
			t.Errorf("-c for %q did not reach tmux as %q", tc.cwd, tc.want)
		}
	}
}

// TestStartLandsInAHostileDirectoryIsolated is forgectl#839 through `forgectl
// launch`'s adapter on a real tmux. A directory holding `#(cmd)` must start
// no job, and the session must start in exactly that directory, as must one
// holding `#{...}` or ending in ';'.
//
// It skips without tmux unless FORGECTL_REQUIRE_TMUX is set, as CI sets it.
//
// Mutation that turns it red: pass spec.CWD() unmapped at the create's -c
// (the session path differs, and the marker appears).
func TestStartLandsInAHostileDirectoryIsolated(t *testing.T) {
	tmuxPath, err := osexec.LookPath("tmux")
	if err != nil {
		if os.Getenv("FORGECTL_REQUIRE_TMUX") != "" {
			t.Fatalf("FORGECTL_REQUIRE_TMUX=1 but tmux is not installed: %v", err)
		}
		t.Skip("tmux not installed")
	}
	tmp, err := os.MkdirTemp("/tmp", "f839-adapter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	// Canonical and symlink-free, because NewStartSpec requires a clean
	// absolute path and macOS's /tmp is a link.
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	sockRoot := filepath.Join(root, "s")
	if err := os.Mkdir(sockRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TMUX_TMPDIR": sockRoot}
	a, err := New(exec.NewOSSensitiveRunner(), tmuxPath, func(k string) string { return env[k] }, os.Getuid)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _, _ = exec.OSRunner{}.Run(context.Background(), tmuxPath, "-S", a.socket, "kill-server") })

	marker := filepath.Join(root, "ran")
	boot, err := backend.NewBootstrapCommand(exec.Opaque("sleep 60"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range []string{"a#(touch " + marker + ")", "b#{session_name}", "c;"} {
		dir := filepath.Join(root, leaf)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		tag, err := backend.NewRecoveryTag()
		if err != nil {
			t.Fatal(err)
		}
		spec, err := backend.NewStartSpec(dir, "forgectl", tag, boot)
		if err != nil {
			t.Fatalf("NewStartSpec(%q): %v", dir, err)
		}
		res := a.Start(context.Background(), spec)
		if res.Outcome() != backend.RefKnown {
			t.Fatalf("Start(%q) outcome = %v, want ref-known (%v)", dir, res.Outcome(), res)
		}
		out, err := exec.OSRunner{}.Run(t.Context(), tmuxPath, "-S", a.socket, "display-message", "-p",
			"-t", "="+spec.OwnershipName()+":", "#{session_path}")
		if err != nil {
			t.Fatalf("display-message: %v", err)
		}
		if got := strings.TrimRight(out, "\n"); got != dir {
			t.Errorf("session started in %q, want exactly %q", got, dir)
		}
	}

	// A `#(...)` job runs asynchronously in the server; give it time to.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a launch directory holding #(touch ...) ran the command in the tmux server")
	}
}
