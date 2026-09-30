package herdr

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

type fakeInfo struct{ mode fs.FileMode }

func (fakeInfo) Name() string                { return "herdr.sock" }
func (fakeInfo) Size() int64                 { return 0 }
func (f fakeInfo) Mode() fs.FileMode         { return f.mode }
func (fakeInfo) ModTime() time.Time          { return time.Time{} }
func (fakeInfo) IsDir() bool                 { return false }
func (fakeInfo) Sys() any                    { return nil }
func statSocket(string) (fs.FileInfo, error) { return fakeInfo{mode: fs.ModeSocket}, nil }

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

var inPane = map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": "/run/herdr.sock"}

// helpRunner answers `tab move --help` the way the fork does: exit 2, usage on stderr.
func forkRunner(t *testing.T) *exec.FakeRunner {
	t.Helper()
	return runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 2, Stderr: fixture(t, "probe_tab_move_stderr.txt")})
}

func TestProbeAcceptsTheForkExactlyAsMeasured(t *testing.T) {
	r := forkRunner(t)
	if err := probe(context.Background(), r, envOf(inPane), statSocket); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if want := []string{"tab", "move", "--help"}; !reflect.DeepEqual(r.Last().Args, want) {
		t.Errorf("argv = %v, want %v", r.Last().Args, want)
	}
}

func TestProbeAcceptsClapStyleHelpOnStdout(t *testing.T) {
	// Synthetic: a clap-style rewrite would exit 0 and print "Usage: ..." on stdout.
	r := runnerFor("Move a tab\n\nUsage: herdr tab move [OPTIONS] <TAB_ID>\n", nil)
	if err := probe(context.Background(), r, envOf(inPane), statSocket); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestProbeRejectsAnyOtherExit2(t *testing.T) {
	for name, stderr := range map[string]string{
		// Real: the fork's reply to an unknown subcommand lists the move lines but has no usage line.
		"unknown subcommand listing the fork's verbs": fixture(t, "probe_unknown_stderr.txt"),
		// Synthetic: how a clap-based stock herdr would refuse a verb it lacks.
		"stock-style unrecognized subcommand": "error: unrecognized subcommand 'move'\n\nUsage: herdr tab [COMMAND]\n",
		"empty stderr":                        "",
	} {
		t.Run(name, func(t *testing.T) {
			r := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 2, Stderr: stderr})
			err := probe(context.Background(), r, envOf(inPane), statSocket)
			if !errors.Is(err, ErrForkRequired) {
				t.Fatalf("err = %v, want ErrForkRequired", err)
			}
		})
	}
}

func TestProbeRejectsExit0WithoutAUsageLine(t *testing.T) {
	r := runnerFor(fixture(t, "probe_tab_help_stdout.txt"), nil)
	if err := probe(context.Background(), r, envOf(inPane), statSocket); !errors.Is(err, ErrForkRequired) {
		t.Fatalf("err = %v, want ErrForkRequired", err)
	}
}

func TestProbeOtherFailuresAreNotEvidenceAboutTheVerb(t *testing.T) {
	for name, err := range map[string]error{
		"exit 1":           &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: "boom"},
		"binary not found": &exec.CommandError{Name: Binary, ExitCode: -1, Err: errors.New("executable file not found")},
		"plain error":      errors.New("context deadline exceeded"),
	} {
		t.Run(name, func(t *testing.T) {
			got := probe(context.Background(), runnerFor("", err), envOf(inPane), statSocket)
			if got == nil || errors.Is(got, ErrForkRequired) {
				t.Fatalf("err = %v, want a failure that is not ErrForkRequired", got)
			}
		})
	}
}

func TestProbeGate(t *testing.T) {
	fork := forkRunner(t)
	for name, tt := range map[string]struct {
		env  map[string]string
		stat func(string) (fs.FileInfo, error)
	}{
		"HERDR_ENV unset":          {map[string]string{"HERDR_SOCKET_PATH": "/run/herdr.sock"}, statSocket},
		"HERDR_ENV empty":          {map[string]string{"HERDR_ENV": "", "HERDR_SOCKET_PATH": "/run/herdr.sock"}, statSocket},
		"HERDR_ENV not 1":          {map[string]string{"HERDR_ENV": "0", "HERDR_SOCKET_PATH": "/run/herdr.sock"}, statSocket},
		"socket path unset":        {map[string]string{"HERDR_ENV": "1"}, statSocket},
		"socket path empty":        {map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": ""}, statSocket},
		"socket path unreadable":   {inPane, func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist }},
		"socket path not a socket": {inPane, func(string) (fs.FileInfo, error) { return fakeInfo{mode: 0o644}, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			if err := probe(context.Background(), fork, envOf(tt.env), tt.stat); !errors.Is(err, ErrNotInSession) {
				t.Fatalf("err = %v, want ErrNotInSession", err)
			}
		})
	}
}

func TestProbeFailureMessageCarriesNoControlCharacters(t *testing.T) {
	// A herdr failure that is neither the usage exit nor a launch failure puts its
	// stderr in the message; that text can echo pane-controlled values.
	r := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: "bad\x1b[31m red\x07"})
	err := probe(context.Background(), r, envOf(inPane), statSocket)
	if err == nil {
		t.Fatal("exit 1 accepted")
	}
	for _, c := range err.Error() {
		if c == 0x1b || c == 0x07 {
			t.Fatalf("message %q contains control character %U", err.Error(), c)
		}
	}
}

func TestProbeVerbMustEndAtTheVerb(t *testing.T) {
	for name, stderr := range map[string]string{
		"a longer verb":     "usage: herdr tab move-all <tab_id>",
		"a longer verb (2)": "Usage: herdr tab moves <tab_id>",
	} {
		r := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 2, Stderr: stderr})
		if err := probe(context.Background(), r, envOf(inPane), statSocket); !errors.Is(err, ErrForkRequired) {
			t.Errorf("%s: err = %v, want ErrForkRequired", name, err)
		}
	}
}

func TestProbeStatFailureKeepsItsCause(t *testing.T) {
	stat := func(string) (fs.FileInfo, error) { return nil, os.ErrPermission }
	err := probe(context.Background(), forkRunner(t), envOf(inPane), stat)
	if !errors.Is(err, ErrNotInSession) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want both ErrNotInSession and the stat error", err)
	}
}

// probe is the gate order a mutating caller follows: CheckSession, then
// CheckFork. The CLI runs the two separately; the tests drive the pair
// through a fake stat.
func probe(ctx context.Context, r exec.Runner, lookupEnv func(string) (string, bool), stat func(string) (fs.FileInfo, error)) error {
	if err := checkSession(lookupEnv, stat); err != nil {
		return err
	}
	return CheckFork(ctx, r)
}

// TestCheckSessionThroughTheRealStat drives the exported CheckSession, the
// only code that passes os.Stat, against a real unix socket. macOS caps a
// socket path near 104 bytes and t.TempDir() overflows it, so the socket
// lives under a short os.MkdirTemp directory.
func TestCheckSessionThroughTheRealStat(t *testing.T) {
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	env := envOf(map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": sock})
	if err := CheckSession(env); err != nil {
		t.Fatalf("CheckSession with a real socket: %v", err)
	}
	notASocket := filepath.Join(dir, "plain")
	if err := os.WriteFile(notASocket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env = envOf(map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": notASocket})
	if err := CheckSession(env); !errors.Is(err, ErrNotInSession) {
		t.Fatalf("CheckSession with a regular file: err = %v, want ErrNotInSession", err)
	}
}

func TestCheckSessionRunsNoHerdrAndSeesTheGate(t *testing.T) {
	if err := checkSession(envOf(inPane), statSocket); err != nil {
		t.Errorf("checkSession in a pane: %v", err)
	}
	if err := checkSession(envOf(map[string]string{}), statSocket); !errors.Is(err, ErrNotInSession) {
		t.Errorf("checkSession outside a pane: err = %v, want ErrNotInSession", err)
	}
	// The exported form reads the real filesystem; a socket path that does not
	// exist must fail as a missing session, not pass.
	env := envOf(map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": "/nonexistent/herdr.sock"})
	if err := CheckSession(env); !errors.Is(err, ErrNotInSession) {
		t.Errorf("CheckSession with a dead socket: err = %v, want ErrNotInSession", err)
	}
}

func TestCheckForkChecksOnlyTheCapability(t *testing.T) {
	if err := CheckFork(context.Background(), forkRunner(t)); err != nil {
		t.Errorf("CheckFork on the fork: %v", err)
	}
	stock := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 2, Stderr: "error: unknown subcommand"})
	if err := CheckFork(context.Background(), stock); !errors.Is(err, ErrForkRequired) {
		t.Errorf("CheckFork on stock herdr: err = %v, want ErrForkRequired", err)
	}
}

func TestProbeGateErrorWinsAndSkipsTheRunner(t *testing.T) {
	// Both the gate and the capability check would fail; the gate is reported and herdr is never run.
	stockish := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 2, Stderr: ""})
	err := probe(context.Background(), stockish, envOf(map[string]string{}), statSocket)
	if !errors.Is(err, ErrNotInSession) || errors.Is(err, ErrForkRequired) {
		t.Fatalf("err = %v, want only ErrNotInSession", err)
	}
	if len(stockish.Calls) != 0 {
		t.Errorf("herdr ran %d time(s) despite a failed gate", len(stockish.Calls))
	}
}

// TestCheckForkCapsFailingStderr is #831: the exit arm echoed herdr's whole
// stderr, escaped but never capped, so a chatty or hostile child could flood
// the terminal through the error. The echo now stops at
// forkProbeStderrMaxRunes and says so, and the head survives.
//
// Mutation: swap termsafe.SafeLineMax back to printable in CheckFork's exit
// arm and the length check fails with the whole 100k-rune stderr in the text.
func TestCheckForkCapsFailingStderr(t *testing.T) {
	stderr := "error: herdr exploded\n" + strings.Repeat("x\u202e", 50_000)
	probe := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 3, Stderr: stderr})
	err := CheckFork(context.Background(), probe)
	if err == nil {
		t.Fatal("CheckFork: nil error for a failing probe")
	}
	got := err.Error()
	if n := utf8.RuneCountInString(got); n > forkProbeStderrMaxRunes+200 {
		t.Errorf("CheckFork error is %d runes; want at most about %d", n, forkProbeStderrMaxRunes)
	}
	if !strings.Contains(got, "error: herdr exploded") || !strings.HasSuffix(got, termsafe.TruncatedMarker) {
		t.Errorf("CheckFork error = %q; want the head kept and the truncation marked", got)
	}
}

// TestCheckForkMarksDroppedStderrStart is #837: exec keeps only the last
// 64 KiB of stderr, so when it dropped the start, the head CheckFork echoes
// is not herdr's first line, and nothing said so. The echo now carries
// exec's own "earlier bytes dropped" marker, and an untruncated stream does
// not.
//
// Mutation: delete the StderrDropped branch in CheckFork and the first row
// loses the marker.
func TestCheckForkMarksDroppedStderrStart(t *testing.T) {
	for _, tt := range []struct {
		dropped int64
		want    bool
	}{{dropped: 4096, want: true}, {dropped: 0, want: false}} {
		probe := runnerFor("", &exec.CommandError{Name: Binary, ExitCode: 3, Stderr: "mid-stream text", StderrDropped: tt.dropped})
		err := CheckFork(context.Background(), probe)
		if err == nil {
			t.Fatal("CheckFork: nil error for a failing probe")
		}
		got := err.Error()
		if marked := strings.Contains(got, "[stderr truncated, 4096 earlier bytes dropped] mid-stream text"); marked != tt.want {
			t.Errorf("dropped=%d: CheckFork error = %q; want marker %v", tt.dropped, got, tt.want)
		}
		if !tt.want && strings.Contains(got, "truncated") {
			t.Errorf("dropped=0: CheckFork error = %q marks a truncation that did not happen", got)
		}
	}
}

// TestCheckSessionQuotesAndCapsTheSocketPath is forgectl#844: both failure
// messages printed HERDR_SOCKET_PATH raw with %s, so a planted value reached
// the terminal escaped only by the root backstop and never capped.
//
// Mutation that turns it red: print sock with a bare %s again on either
// line (the stat-failure or the not-a-socket message).
func TestCheckSessionQuotesAndCapsTheSocketPath(t *testing.T) {
	sock := "/run/" + strings.Repeat("a", 2*termsafe.PathEchoMaxRunes) + "\x1b[31m/herdr.sock"
	env := envOf(map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": sock})
	for name, stat := range map[string]func(string) (fs.FileInfo, error){
		"unreadable": func(p string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrPermission}
		},
		"not a socket": func(string) (fs.FileInfo, error) { return fakeInfo{}, nil },
	} {
		t.Run(name, func(t *testing.T) {
			err := checkSession(env, stat)
			if !errors.Is(err, ErrNotInSession) {
				t.Fatalf("err = %v, want ErrNotInSession", err)
			}
			msg := err.Error()
			if strings.Contains(msg, sock) || strings.Contains(msg, "\x1b") {
				t.Errorf("the socket path reached the message raw: %q", msg)
			}
			if !strings.Contains(msg, termsafe.QuotePath(sock)) {
				t.Errorf("message %q does not carry the capped, quoted socket path", msg)
			}
		})
	}
}
