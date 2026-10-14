// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]bool{
		"0.33.0": true, "v1.2.3": true, "0.45.0-rc.1": true, "1.0.0+abc": true,
		"0.45.0 (abc1234)": true, "v0.45.0 (abc1234)": true,
		"dev": false, "": false, "1.2": false, "1.x.3": false, "1.2.3.4": false, "dev (abc1234)": false,
	} {
		if _, ok := parseVersion(in); ok != want {
			t.Errorf("parseVersion(%q) ok = %v, want %v", in, ok, want)
		}
	}
	older, _ := parseVersion("0.29.0")
	for _, s := range []string{"0.33.0", "0.29.1", "1.0.0", "v0.30.0"} {
		newer, _ := parseVersion(s)
		if !versionLess(older, newer) || versionLess(newer, older) {
			t.Errorf("0.29.0 should be older than %s", s)
		}
	}
	if v, _ := parseVersion("0.45.0 (abc1234)"); v != [3]int{0, 45, 0} {
		t.Errorf("a release with its commit parsed as %v", v)
	}
	if versionLess(older, older) {
		t.Error("a version is older than itself")
	}
}

// fakeForgectl puts a forgectl on PATH that prints version as `version
// --json` does, and returns its path.
func fakeForgectl(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "forgectl")
	script := "#!/bin/sh\n[ \"$1 $2\" = 'version --json' ] || exit 2\nprintf '{\"name\":\"forgectl\",\"version\":\"%s\"}\\n' '" + version + "'\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // G306: the test binary must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return p
}

func TestInstalledNewer(t *testing.T) {
	if installedNewer("dev") != nil {
		t.Error("a dev build should not check")
	}
	p := fakeForgectl(t, "0.33.0")
	check := installedNewer("0.29.0")
	if got := check(t.Context()); got != "0.33.0" {
		t.Errorf("newer installed: got %q, want 0.33.0", got)
	}
	if got := installedNewer("0.33.0")(t.Context()); got != "" {
		t.Errorf("same version installed: got %q, want none", got)
	}
	if got := installedNewer("0.40.0")(t.Context()); got != "" {
		t.Errorf("older installed: got %q, want none", got)
	}
	// A file that changed is asked again; one that did not keeps the answer.
	script := strings.Replace(mustRead(t, p), "0.33.0", "0.29.0", 1)
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // G306: the test binary must be executable
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if got := check(t.Context()); got != "" {
		t.Errorf("after the binary was replaced by the running version: got %q, want none", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got := check(t.Context()); got != "" {
		t.Errorf("no forgectl on PATH: got %q", got)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // G304: a test temp file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The model asks at start, shows the answer under the header, and asks again
// on a timer, never per frame.
func TestDesk_NewerInstalledShowsUnderTheHeader(t *testing.T) {
	h := newDeskHarness(t)
	h.drop("01-a.sh", plainScript("a"))
	h.scan()
	if h.m.newerCmd(0) != nil {
		t.Fatal("a desk with no check asks anyway")
	}
	calls := 0
	h.m.opts.NewerInstalled = func(context.Context) string { calls++; return "0.33.0" }
	msg := h.m.newerCmd(0)()
	if calls != 1 {
		t.Fatalf("the first check ran %d times", calls)
	}
	out, next := h.m.Update(msg)
	h.m = out.(deskModel) //nolint:forcetypeassert // test
	if next == nil {
		t.Error("no next check was scheduled")
	}
	view := ansi.Strip(h.m.View().Content)
	if !strings.Contains(view, "newer forgectl installed (0.33.0) · q then forgectl desk to restart") {
		t.Errorf("no notice under the header:\n%s", view)
	}
	_ = h.m.View()
	if calls != 1 {
		t.Errorf("drawing a frame ran the check: %d calls", calls)
	}
	out, _ = h.m.Update(deskNewerMsg{})
	h.m = out.(deskModel) //nolint:forcetypeassert // test
	if strings.Contains(ansi.Strip(h.m.View().Content), "newer forgectl") {
		t.Error("the notice stayed after a check found nothing newer")
	}
}

// scriptForgectl puts a forgectl on PATH (ahead of the system dirs, for the
// tools the script calls) whose body is the shell script given.
func scriptForgectl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "forgectl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // G306: the test binary must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return p
}

// A release prints its commit after the version, as fang's WithCommit puts
// it; the notice still fires, and shows the version alone.
func TestInstalledNewer_ReleaseWithCommit(t *testing.T) {
	fakeForgectl(t, "0.45.0 (abc1234)")
	if got := installedNewer("0.44.0")(t.Context()); got != "0.45.0" {
		t.Errorf("got %q, want 0.45.0", got)
	}
}

// A run that fails is remembered under the same file, so a broken binary is
// not run every minute, and the previous answer stays rather than flicker.
func TestInstalledNewer_FailureKeepsTheAnswerAndIsNotRerun(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	p := fakeForgectl(t, "0.33.0")
	check := installedNewer("0.29.0")
	if got := check(t.Context()); got != "0.33.0" {
		t.Fatalf("first check: got %q", got)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho x >> '"+count+"'\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: the test binary must be executable
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := check(t.Context()); got != "0.33.0" {
			t.Errorf("a failed run cleared the answer: got %q", got)
		}
	}
	if runs := strings.Count(mustRead(t, count), "x"); runs != 1 {
		t.Errorf("the broken binary ran %d times, want once", runs)
	}
}

// Output past the cap is an error, not a version.
func TestBinaryVersion_CapsOutput(t *testing.T) {
	p := scriptForgectl(t, "head -c 100000 /dev/zero | tr '\\0' a\n")
	if _, err := binaryVersion(t.Context(), p); err == nil {
		t.Error("100 KB of output read as a version")
	}
}

// A wrapper whose child holds stdout open does not hold the check past its
// timeout and the wait delay.
func TestBinaryVersion_ChildHoldingStdoutDoesNotHang(t *testing.T) {
	p := scriptForgectl(t, "sleep 30 &\nsleep 30\n")
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := binaryVersion(ctx, p); err == nil {
		t.Error("a run killed at its timeout read as a version")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the check took %v", took)
	}
}

// A first run that times out (a slow first launch) is tried again on a later
// tick for the same file, so the upgrade still shows (#1180 review).
func TestInstalledNewer_TimedOutRunIsRetried(t *testing.T) {
	prev := deskNewerRetry
	deskNewerRetry = 0
	t.Cleanup(func() { deskNewerRetry = prev })
	slow := filepath.Join(t.TempDir(), "slow")
	if err := os.WriteFile(slow, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	scriptForgectl(t, "[ -e '"+slow+"' ] && exec sleep 30\nprintf '{\"version\":\"0.33.0 (abc1234)\"}\\n'\n")
	check := installedNewer("0.29.0")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if got := check(ctx); got != "" {
		t.Fatalf("a timed-out run answered %q", got)
	}
	if err := os.Remove(slow); err != nil {
		t.Fatal(err)
	}
	if got := check(t.Context()); got != "0.33.0" {
		t.Errorf("the retry for the same file answered %q, want 0.33.0", got)
	}
	// The answer stays while forgectl is briefly off PATH mid-upgrade.
	t.Setenv("PATH", t.TempDir())
	if got := check(t.Context()); got != "0.33.0" {
		t.Errorf("no forgectl on PATH cleared the answer: %q", got)
	}
}

// A wrapper that prints its version and exits, leaving a child holding
// stdout, still answers, and the child is stopped.
func TestBinaryVersion_LeftoverChildStillAnswers(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := scriptForgectl(t, "printf '{\"version\":\"0.33.0\"}\\n'\nsleep 30 &\necho $! > '"+pidFile+"'\n")
	start := time.Now()
	got, err := binaryVersion(t.Context(), p)
	if err != nil || got != "0.33.0" {
		t.Fatalf("binaryVersion = %q, %v; want 0.33.0", got, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the check took %v", took)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(mustRead(t, pidFile)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the leftover child %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
