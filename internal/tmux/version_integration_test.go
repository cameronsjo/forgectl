//go:build unix

package tmux

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

func TestGenerationCapabilityIsolated(t *testing.T) {
	// skipOrFail, not t.Skip: this test measures a real server's generation
	// fields, so where it is meant to run (CI, FORGECTL_REQUIRE_TMUX=1) a skip
	// must not read as a pass. See target_integration_test.go.
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		skipOrFail(t, "tmux not installed")
	}
	versionOut, err := internalexec.OSRunner{}.Run(context.Background(), tmuxBin, "-V")
	if err != nil {
		skipOrFail(t, "tmux -V: %v", err)
	}
	major, minor, _, err := parseTmuxVersion(versionOut)
	if err != nil || major < 2 || (major == 2 && minor < 2) {
		skipOrFail(t, "tmux 2.2+ required: %q", versionOut)
	}

	// Not t.TempDir(): macOS caps a Unix socket path at ~104 bytes and
	// t.TempDir() embeds the full test name, so tmux's <root>/tmux-<uid>/default
	// overflows it. "/tmp" is absolute and filepath.Clean-stable on both macOS
	// and Linux, which is what classifyServerFailure requires of TMUX_TMPDIR.
	root, err := os.MkdirTemp("/tmp", "f242-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", root)
	ctx := context.Background()
	runner := internalexec.OSRunner{}
	c := New(runner, WithBins(tmuxBin, "sesh"))
	socketPath := filepath.Join(root, "tmux-"+strconv.Itoa(os.Geteuid()), "default")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), tmuxBin, "kill-server") })

	windows, err := c.ListWindows(ctx)
	if err != nil || len(windows) != 0 {
		t.Fatalf("absent ListWindows = (%+v,%v), want empty,nil", windows, err)
	}
	if _, err := c.CheckGenerationCapability(ctx); err != nil {
		t.Fatalf("absent capability: %v", err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("absent probe left socket %q: %v", socketPath, err)
	}

	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "isolated", "sleep 30"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	if _, err := c.CheckGenerationCapability(ctx); err != nil {
		t.Fatalf("active capability: %v", err)
	}
	if _, err := runner.Run(ctx, tmuxBin, "kill-server"); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("bind stale socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CheckGenerationCapability(ctx); err == nil {
		t.Fatal("stale socket capability succeeded, want refusal")
	}
}
