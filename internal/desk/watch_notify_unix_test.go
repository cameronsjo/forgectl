// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package desk

import (
	"os"
	"testing"
	"time"
)

// WatchNotify calls onState on the first poll and only when the state changes,
// and a nil onState leaves Watch's behavior as it was.
func TestWatchNotify_ReportsStateOnceAndOnChange(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "notify.sh", "echo hi\n")
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(0, "exit"); err != nil {
		t.Fatal(err)
	}
	var states []WatchState
	state, _, err := d.WatchNotify(t.Context(), c.Name, 0, 10*time.Millisecond, func(string) {}, func(s WatchState) { states = append(states, s) })
	if err != nil || state != WatchEnded {
		t.Fatalf("state %s err %v, want ended", state, err)
	}
	if len(states) != 1 || states[0] != WatchEnded {
		t.Errorf("onState calls = %v, want exactly one for the first poll", states)
	}
}

func TestWatchNotify_NilCallbackMatchesWatch(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "nilcb.sh", "echo hi\n")
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(0, "exit"); err != nil {
		t.Fatal(err)
	}
	state, seen, err := d.WatchNotify(t.Context(), c.Name, 0, 10*time.Millisecond, func(string) {}, nil)
	if err != nil || state != WatchEnded || seen != 2 {
		t.Errorf("state %s seen %d err %v, want ended 2 nil", state, seen, err)
	}
}
