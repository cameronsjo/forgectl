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

// slowWriter takes 100ms per write, like a terminal that is slow to drain.
type slowWriter struct{}

func (slowWriter) Write(p []byte) (int, error) {
	time.Sleep(100 * time.Millisecond)
	return len(p), nil
}

// TestReportOnSignal_DiesBySignal pins the handler's contract in a child
// process: on SIGTERM it reports, then the process must die BY the signal even
// when the command's own return path (stop, then exit 1) races the handler.
func TestReportOnSignal_DiesBySignal(t *testing.T) {
	if os.Getenv("FORGECTL_SIGNAL_HELPER") == "1" {
		// A slow write holds the handler mid-report while the command returns.
		r := progstatus.ForWriter(slowWriter{}, false)
		stop := reportOnSignal(r, "helper")
		fmt.Println("ready")
		// Simulate the command returning promptly once its children die from
		// the same Ctrl-C: stop, then a normal exit(1).
		time.Sleep(60 * time.Millisecond)
		stop()
		os.Exit(1)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReportOnSignal_DiesBySignal$")
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
