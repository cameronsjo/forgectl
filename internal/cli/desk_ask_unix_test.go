//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingNotifier stands in for hc in every ask test, so none reaches a
// real terminal.
type recordingNotifier struct {
	mu    sync.Mutex
	sends []string
}

func (r *recordingNotifier) Notify(_ context.Context, addr, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, addr+" | "+text)
	return nil
}

func stubAskNotifier(t *testing.T) *recordingNotifier {
	t.Helper()
	n := &recordingNotifier{}
	prev := deskAskNotifier
	deskAskNotifier = n
	t.Cleanup(func() { deskAskNotifier = prev })
	return n
}

func askID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(line, "id="); ok {
			return id
		}
	}
	t.Fatalf("no id= line in %q", out)
	return ""
}

func TestDeskAsk_RefusesUnsafeInput(t *testing.T) {
	newDeskDir(t)
	stubAskNotifier(t)
	cases := [][]string{
		{"ask"},
		{"ask", "--title", "x", "--link", "javascript:alert(1)"},
		{"ask", "--title", "x", "--bucket", "urgent"},
		{"ask", "--title", "x", "--allow", "approve"},
		{"ask", "--title", "x", "--notify", "self"},
		{"ask", "--title", "two\nlines"},
	}
	for _, args := range cases {
		_, _, err := deskRun(t, deskDeps(), args...)
		wantExit(t, err, deskExitUsage)
	}
}

func TestDeskAsk_RespondNotifiesAndWatchEnds(t *testing.T) {
	newDeskDir(t)
	n := stubAskNotifier(t)
	stubDeskEnv(t, map[string]string{"DESK_DIR": deskDirFromEnv(t), "HC_TERMINAL_ID": "t9z", "FORGECTL_WORKER": "cos-test"})
	steps := writeTemp(t, "steps.md", "1. Open settings\n2. Rename the key\n")
	out, _, err := deskRun(t, deskDeps(), "ask", "--title", "Rename the key", "--steps-file", steps,
		"--bucket", "now", "--link", "Docs=https://example.com/a", "--notify", "self")
	wantExit(t, err, 0)
	id := askID(t, out)
	if !strings.Contains(out, "watch=forgectl desk watch --responses "+id) {
		t.Fatalf("no watch= line: %q", out)
	}

	out, _, err = deskRun(t, deskDeps(), "respond", id, "reply", "use the staging account")
	wantExit(t, err, 0)
	if !strings.Contains(out, "state=answered") || !strings.Contains(out, "notified=t9z") {
		t.Fatalf("respond out = %q", out)
	}
	if len(n.sends) != 1 || !strings.HasPrefix(n.sends[0], "t9z | DESK-RESPONSE "+id+" reply: use the staging account") {
		t.Fatalf("sends = %q", n.sends)
	}

	// Answered before the watch started: it prints the response and exits 0.
	out, _, err = deskRun(t, deskDeps(), "watch", "--responses", id, "--deadline", "5")
	wantExit(t, err, 0)
	var line askWatchLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &line); err != nil {
		t.Fatalf("watch line %q: %v", out, err)
	}
	if line.Ask != id || line.Response.Kind != "reply" || line.State != "answered" {
		t.Fatalf("watch line = %+v", line)
	}

	// A second answer is refused.
	_, _, err = deskRun(t, deskDeps(), "respond", id, "done")
	wantExit(t, err, 1)

	out, _, err = deskRun(t, deskDeps(), "asks", id)
	wantExit(t, err, 0)
	for _, want := range []string{"state=answered", "from=cos-test", "notified=resp-"} {
		if !strings.Contains(out, want) {
			t.Errorf("asks detail lacks %q:\n%s", want, out)
		}
	}
}

func TestDeskWatchResponses_DeadlineAndUnknown(t *testing.T) {
	newDeskDir(t)
	stubAskNotifier(t)
	prev := deskWatchInterval
	deskWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { deskWatchInterval = prev })

	out, _, err := deskRun(t, deskDeps(), "ask", "--title", "Wait for me")
	wantExit(t, err, 0)
	id := askID(t, out)

	out, _, err = deskRun(t, deskDeps(), "watch", "--responses", id, "--deadline", "1")
	wantExit(t, err, deskExitTempFail)
	if !strings.Contains(out, "resume=forgectl desk watch --responses "+id+" --since ") {
		t.Fatalf("no resume line: %q", out)
	}

	_, _, err = deskRun(t, deskDeps(), "watch", "--responses", "ask-00000000", "--deadline", "1")
	wantExit(t, err, 1)
	if err == nil || !strings.Contains(err.Error(), "no ask ask-00000000") {
		t.Fatalf("unknown ask err = %v", err)
	}

	_, _, err = deskRun(t, deskDeps(), "watch", "--responses", "not an id")
	wantExit(t, err, deskExitUsage)

	_, _, err = deskRun(t, deskDeps(), "watch", "--since", "3", "01-x")
	wantExit(t, err, deskExitUsage)
}

func TestParseSnooze(t *testing.T) {
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "4h": 4 * time.Hour, "2d": 48 * time.Hour} {
		got, err := parseSnooze(in)
		if err != nil || got != want {
			t.Errorf("parseSnooze(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0h", "-1h", "xd", "soon"} {
		if _, err := parseSnooze(bad); err == nil {
			t.Errorf("parseSnooze(%q) accepted", bad)
		}
	}
}

// deskDirFromEnv reads back the DESK_DIR newDeskDir set.
func deskDirFromEnv(t *testing.T) string {
	t.Helper()
	d := deskGetenv("DESK_DIR")
	if d == "" {
		t.Fatal("DESK_DIR not stubbed")
	}
	return d
}

// TestDeskWatchResponses_UnscopedStartsAtTheEndAndResumes pins the unscoped
// watch: a fresh one prints only new responses, and a resume past an ask's
// record still names its title and state.
func TestDeskWatchResponses_UnscopedStartsAtTheEndAndResumes(t *testing.T) {
	newDeskDir(t)
	stubAskNotifier(t)
	prev := deskWatchInterval
	deskWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { deskWatchInterval = prev })

	out, _, err := deskRun(t, deskDeps(), "ask", "--title", "Old one")
	wantExit(t, err, 0)
	old := askID(t, out)
	_, _, err = deskRun(t, deskDeps(), "respond", old, "done")
	wantExit(t, err, 0)
	out, _, err = deskRun(t, deskDeps(), "ask", "--title", "New one")
	wantExit(t, err, 0)
	fresh := askID(t, out)

	// A fresh unscoped watch does not replay the old answer.
	out, _, err = deskRun(t, deskDeps(), "watch", "--responses", "--deadline", "1")
	wantExit(t, err, deskExitTempFail)
	if strings.Contains(out, old) {
		t.Fatalf("a fresh unscoped watch replayed history: %q", out)
	}
	var since string
	for _, line := range strings.Split(out, "\n") {
		if r, ok := strings.CutPrefix(line, "resume="); ok {
			f := strings.Fields(r)
			for i := range f {
				if f[i] == "--since" && i+1 < len(f) {
					since = f[i+1]
				}
			}
		}
	}
	if since == "" {
		t.Fatalf("no --since in %q", out)
	}

	// Answer after the resume point; the resumed watch names the ask.
	_, _, err = deskRun(t, deskDeps(), "respond", fresh, "reply", "on it")
	wantExit(t, err, 0)
	out, _, err = deskRun(t, deskDeps(), "watch", "--responses", "--since", since, "--deadline", "1")
	wantExit(t, err, deskExitTempFail)
	var line askWatchLine
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &line); err != nil {
		t.Fatalf("first line of %q: %v", out, err)
	}
	if line.Ask != fresh || line.Title != "New one" || line.State != "answered" {
		t.Fatalf("resumed line = %+v", line)
	}
	if strings.Count(out, "\"response\"") != 1 {
		t.Fatalf("resume printed more than the new response: %q", out)
	}
}
