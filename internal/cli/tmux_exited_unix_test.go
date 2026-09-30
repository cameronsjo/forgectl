//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// exitedServerRunner is the state tmux 3.4 leaves after its server exits
// (forgectl#786): the default socket file exists in a private TMUX_TMPDIR, a
// connect to it is refused, and every tmux command fails exit 1 with "no server
// running". The socket is real, so the tmux client's own lstat and connect
// probe classify it; only the tmux commands are faked.
func exitedServerRunner(t *testing.T) *exec.FakeRunner {
	t.Helper()
	// Not t.TempDir(): a unix socket path is capped near 104 bytes on macOS.
	root, err := os.MkdirTemp("/tmp", "f805-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", root)
	socketPath := filepath.Join(root, "tmux-"+strconv.Itoa(os.Getuid()), "default")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("bind the leftover socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", &exec.CommandError{
			Name: name, Args: append([]string(nil), args...), ExitCode: 1,
			Stderr: "no server running on " + socketPath, Err: errors.New("exit status 1"),
		}
	}}
}

// TestTmuxKillAndRenameOnAnExitedServer is forgectl#805 item 3. Over an exited
// server's leftover socket both verbs still fail, but say there is no server
// rather than that its state "could not be read", and the remedy rides along.
//
// Mutation that turns it red: drop either command's ErrServerExited arm (the
// message is the strict resolve's "could not be read" again).
func TestTmuxKillAndRenameOnAnExitedServer(t *testing.T) {
	for name, args := range map[string][]string{
		"kill":   {"--yes", "work"},
		"rename": {"work", "new"},
	} {
		t.Run(name, func(t *testing.T) {
			client := tmux.New(exitedServerRunner(t))
			cmd := newTmuxRenameCmd(client)
			if name == "kill" {
				cmd = newTmuxKillCmd(client, theme.Theme{})
			}
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(context.Background())
			if err == nil {
				t.Fatal("expected an error over an exited server")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "no tmux server is running") || strings.Contains(msg, "could not be read") {
				t.Errorf("error = %q, want the no-server wording, not \"could not be read\"", msg)
			}
			if !strings.Contains(msg, "start any tmux session") {
				t.Errorf("error = %q, want the remedy", msg)
			}
			if !errors.Is(err, tmux.ErrServerExited) {
				t.Errorf("error = %v, want it to wrap tmux.ErrServerExited", err)
			}
			// forgectl#815: the leftover socket is named, so an operator with
			// more than one server knows which one the remedy is about.
			// Mutation that turns it red: drop the ExitedSocketPath arm of
			// noServerForSession.
			wantSocket := "tmux-" + strconv.Itoa(os.Getuid()) + "/default"
			if !strings.Contains(msg, "on socket ") || !strings.Contains(msg, wantSocket) {
				t.Errorf("error = %q, want it to name the leftover socket (%s)", msg, wantSocket)
			}
		})
	}
}

// TestPrListOnAnExitedServerSaysNoTmuxServer is forgectl#805 item 5 end to
// end: `pr list --json` over an exited server's leftover socket reports a live
// record's status as "no tmux server", never "window gone".
//
// Mutation that turns it red: drop prListLiveness's serverExited arm (the row
// reads "window gone" again).
func TestPrListOnAnExitedServerSaysNoTmuxServer(t *testing.T) {
	ref := pr.Ref{Owner: "o", Repo: "r", Number: 4}
	got := runPrListOverJSON(t, exitedServerRunner(t), []pr.Ref{ref}, nil)
	if !strings.Contains(got, `"status": "`+noTmuxServerStatus+`"`) || strings.Contains(got, "window gone") {
		t.Errorf("pr list --json over an exited server =\n%s\nwant status %q", got, noTmuxServerStatus)
	}
}
