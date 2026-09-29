package herdr

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
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
