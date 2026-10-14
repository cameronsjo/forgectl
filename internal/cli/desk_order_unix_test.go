//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// `desk add --after` writes the AFTER line, status shows the item refused
// until the earlier item succeeds, and --json carries the names
// (forgectl#1202).
func TestDeskAdd_AfterHoldsTheItem(t *testing.T) {
	dir := newDeskDir(t)
	enable, enableSHA := queueItem(t, "enable.sh", "echo enable\n")
	out, _, err := deskRun(t, deskDeps(), "add", writeTemp(t, "verify.sh", "echo verify\n"),
		"--what", "verify", "--why", "a test", "--after", enable, "--json")
	wantExit(t, err, 0)
	var a deskAddJSON
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("add --json: %v\n%s", err, out)
	}

	out, _, err = deskRun(t, deskDeps(), "status")
	wantExit(t, err, 0)
	if !strings.Contains(out, "refused  "+a.Name) || !strings.Contains(out, "after="+enable) || !strings.Contains(out, "waits on "+enable) {
		t.Errorf("status before %s ran:\n%s", enable, out)
	}
	out, _, err = deskRun(t, deskDeps(), "status", "--json")
	wantExit(t, err, 0)
	var snap deskStatusJSON
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	var after []string
	for _, it := range snap.Pending {
		if it.Name == a.Name {
			after = it.After
		} else if it.After == nil {
			t.Errorf("%s: after is null, want []", it.Name)
		}
	}
	if !slices.Equal(after, []string{enable}) {
		t.Errorf("status --json after = %q, want [%s]", after, enable)
	}

	finishRun(t, openTestDesk(t, dir), enable, enableSHA, 0)
	out, _, err = deskRun(t, deskDeps(), "status")
	wantExit(t, err, 0)
	if !strings.Contains(out, "waiting  "+a.Name) {
		t.Errorf("status after %s succeeded:\n%s", enable, out)
	}

	_, _, err = deskRun(t, deskDeps(), "add", writeTemp(t, "x.sh", "true\n"), "--what", "x", "--why", "y", "--after", "enable")
	wantExit(t, err, deskExitUsage)
}

// The status line cuts WHAT at deskWhatCols; --full prints it whole.
func TestDeskStatus_FullPrintsTheWholeWhat(t *testing.T) {
	newDeskDir(t)
	what := "run only between :35 and :59 " + strings.Repeat("x", 80) + " END"
	_, _, err := deskRun(t, deskDeps(), "add", writeTemp(t, "long.sh", "true\n"), "--what", what, "--why", "a test")
	wantExit(t, err, 0)

	out, _, err := deskRun(t, deskDeps(), "status")
	wantExit(t, err, 0)
	if strings.Contains(out, " END") || !strings.Contains(out, "[truncated]") {
		t.Errorf("status without --full should cut WHAT:\n%s", out)
	}
	out, _, err = deskRun(t, deskDeps(), "status", "--full")
	wantExit(t, err, 0)
	if !strings.Contains(out, `what="`+what+`"`) {
		t.Errorf("status --full lacks the whole WHAT:\n%s", out)
	}
}

// With no --deadline and no terminal, watch stops on its own with exit 75
// and a resume= line that carries the deadline it used (forgectl#1094); at
// a terminal, or with --deadline 0, there is no default bound.
func TestDeskWatch_DefaultDeadlineWithoutATerminal(t *testing.T) {
	fastWatch(t)
	newDeskDir(t)
	prevTTY, prevDeadline := deskHasTerminal, deskWatchDefaultDeadline
	t.Cleanup(func() { deskHasTerminal, deskWatchDefaultDeadline = prevTTY, prevDeadline })
	deskHasTerminal = func() bool { return false }
	deskWatchDefaultDeadline = 1

	name, _ := queueItem(t, "wait.sh", "true\n")
	start := time.Now()
	out, _, err := deskRun(t, deskDeps(), "watch", name)
	wantExit(t, err, deskExitTempFail)
	if time.Since(start) > 10*time.Second {
		t.Errorf("watch took %v", time.Since(start))
	}
	if want := "resume=forgectl desk watch " + name + " --skip 0 --deadline 1\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	// --deadline 0 given explicitly, or a terminal: no default bound, so the
	// watch is still waiting when the caller's own 2s context ends it, and
	// it reports an interruption (130), not the deadline (75).
	for _, tc := range []struct {
		tty  bool
		args []string
	}{{false, []string{"--deadline", "0"}}, {true, nil}} {
		deskHasTerminal = func() bool { return tc.tty }
		for _, k := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
			t.Setenv(k, "")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		cmd := newDeskCmd(module.Deps{Theme: theme.Default(), Runner: &exec.FakeRunner{}, SensitiveRunner: &exec.FakeSensitiveRunner{}})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"watch", name}, tc.args...))
		err := cmd.ExecuteContext(ctx)
		cancel()
		wantExit(t, err, deskExitInterrupted)
	}
}
