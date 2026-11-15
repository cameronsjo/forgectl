// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/progstatus"
)

func TestCatchableSignals_SkipsIgnored(t *testing.T) {
	cases := []struct {
		name    string
		ignored map[os.Signal]bool
		want    []os.Signal
	}{
		{"none ignored", nil, []os.Signal{syscall.SIGINT, syscall.SIGTERM}},
		{"SIGINT ignored (background job in a POSIX shell)", map[os.Signal]bool{syscall.SIGINT: true}, []os.Signal{syscall.SIGTERM}},
		{"SIGTERM ignored", map[os.Signal]bool{syscall.SIGTERM: true}, []os.Signal{syscall.SIGINT}},
		{"both ignored", map[os.Signal]bool{syscall.SIGINT: true, syscall.SIGTERM: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := catchableSignals(func(s os.Signal) bool { return tc.ignored[s] })
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// slowWriter takes 100ms per write, like a terminal that is slow to drain, and
// tells the test when its first write has begun: the handler has then taken the
// signal and is mid-report, which is the moment the race needs.
type slowWriter struct {
	started chan struct{}
	once    *sync.Once
}

func (w slowWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	time.Sleep(100 * time.Millisecond)
	return len(p), nil
}

// TestReportOnSignal_DiesBySignal pins the handler's contract in a child
// process: on SIGTERM it reports, then the process must die BY the signal even
// when the command's own return path (stop, then exit 1) races the handler.
func TestReportOnSignal_DiesBySignal(t *testing.T) {
	if os.Getenv("FORGECTL_SIGNAL_HELPER") == "1" {
		// A slow write holds the handler mid-report while the command returns.
		w := slowWriter{started: make(chan struct{}), once: &sync.Once{}}
		r := progstatus.ForWriter(w, false)
		stop := reportOnSignal(r, "helper")
		fmt.Println("ready")
		// Return only once the handler is mid-write, as a command whose
		// children died from the same Ctrl-C would: stop, then a normal exit(1).
		select {
		case <-w.started:
			stop()
			os.Exit(1)
		case <-time.After(10 * time.Second):
			os.Exit(3) // the signal never arrived; not the behavior under test
		}
	}
	// The command is this very test binary with a fixed -test.run pattern; no input reaches it.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReportOnSignal_DiesBySignal$") //nolint:gosec // G204/G702: re-executes this test binary, fixed arguments
	cmd.Env = append(os.Environ(), "FORGECTL_SIGNAL_HELPER=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if !strings.Contains(line, "ready") {
		t.Fatalf("helper said %q, want ready", line)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) {
		t.Fatalf("Wait = %v, want an ExitError", waitErr)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("helper ended with %v, want death by SIGTERM (not exit 1)", ee)
	}
}
