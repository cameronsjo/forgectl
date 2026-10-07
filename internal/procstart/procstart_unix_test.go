//go:build darwin || linux

package procstart

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestMatches(t *testing.T) {
	self := os.Getpid()
	start, err := Of(self)
	if err != nil || start == 0 {
		t.Fatalf("Of(self) = %d, %v", start, err)
	}
	if ok, err := Matches(self, start); !ok || err != nil {
		t.Fatalf("Matches(self, own start) = %v, %v; want true", ok, err)
	}
	if ok, _ := Matches(self, 0); ok {
		t.Error("a zero recorded start time matched; it must refuse")
	}
	if ok, _ := Matches(self, start+1); ok {
		t.Error("a different start time matched; the pid must count as reused")
	}
}

func TestMatchesExitedProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	start, err := Of(pid)
	if err != nil {
		// Exited and reaped already on a fast machine: nothing to compare.
		start = 1
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	ok, err := Matches(pid, start)
	if ok {
		t.Fatal("an exited, reaped process matched")
	}
	if !errors.Is(err, ErrNoProcess) && !errors.Is(err, ErrZombie) {
		t.Logf("err %v (the pid may have been reused)", err)
	}
}
