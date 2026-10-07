package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The package's tests run inside this repository, whose root carries
// .claude/settings.json, so every test that drives launchExec from the package
// directory would really chdir the test process to the root and break each
// later test that reads a file by a relative path. No test here wants the real
// move; launchExecProbe installs its own recorder over this one.
func init() {
	launchChdir = func(string) error { return nil }
}

// settingsRootRepo builds a repository whose root carries .claude settings and
// returns the root and a subfolder of it, both with symlinks resolved so they
// compare equal to what os.Getwd reports after a chdir.
func settingsRootRepo(t *testing.T) (root, sub string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub = filepath.Join(root, "pkg", "sub")
	for _, dir := range []string{filepath.Join(root, ".git"), filepath.Join(root, ".claude"), sub} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, sub
}

// launchRun is what launchExecProbe saw: where launchExec chdir'd ("" for
// nowhere), the argv and env it would exec with, and everything it wrote to
// stdout and stderr.
type launchRun struct {
	moved string
	argv  []string
	env   []string
	out   string
}

// launchExecProbe runs launchExec from dir with the exec and chdir seams
// stubbed.
func launchExecProbe(t *testing.T, dir, harness string, args []string) launchRun {
	t.Helper()
	t.Chdir(dir)
	var run launchRun
	prevExec, prevChdir := execHarness, launchChdir
	execHarness = func(_ string, a, e []string) error {
		run.argv, run.env = a, e
		return nil
	}
	launchChdir = func(d string) error {
		run.moved = d
		return nil
	}
	t.Cleanup(func() { execHarness, launchChdir = prevExec, prevChdir })

	run.out = captureStdio(t, func() {
		if err := launchExec(nil, usageConfig(t, harness, false), args); err != nil {
			t.Errorf("launchExec: %v", err)
		}
	})
	return run
}

// TestLaunchExec_SettingsRoot is cameronsjo/cadence-ecosystem#608 at the CLI:
// `forgectl launch` from a repository subfolder starts claude at the root
// where the .claude settings live, says so on stderr, and `--here` as the
// first argument keeps it put. A harness that does not read .claude never
// moves.
func TestLaunchExec_SettingsRoot(t *testing.T) {
	root, sub := settingsRootRepo(t)

	t.Run("claude moves to the root and says so", func(t *testing.T) {
		run := launchExecProbe(t, sub, "claude", nil)
		if run.moved != root {
			t.Fatalf("chdir to %q, want the repository root %q", run.moved, root)
		}
		if !slices.Contains(run.env, "PWD="+root) {
			t.Errorf("exec env has no PWD=%s", root)
		}
		if !strings.Contains(run.out, "forgectl: starting claude in") || !strings.Contains(run.out, "--here") {
			t.Errorf("stderr = %q, want the one-line move notice naming --here", run.out)
		}
	})

	t.Run("--here stays and is not passed to claude", func(t *testing.T) {
		run := launchExecProbe(t, sub, "claude", []string{"--here", "--", "do it"})
		if run.moved != "" {
			t.Errorf("chdir to %q, want no move with --here", run.moved)
		}
		if strings.Contains(run.out, "starting claude in") {
			t.Errorf("stderr = %q, want no move notice with --here", run.out)
		}
		if slices.Contains(run.argv, "--here") || slices.Contains(run.argv, "--") {
			t.Errorf("argv %q carries forgectl's own --here or separator", run.argv)
		}
		if !slices.Contains(run.argv, "do it") {
			t.Errorf("argv %q lost the prompt", run.argv)
		}
	})

	t.Run("a later --here belongs to the harness", func(t *testing.T) {
		run := launchExecProbe(t, sub, "claude", []string{"--", "--here"})
		if run.moved != root {
			t.Errorf("chdir to %q, want %q: only a leading --here is forgectl's", run.moved, root)
		}
		if !slices.Contains(run.argv, "--here") {
			t.Errorf("argv %q lost the harness's --here", run.argv)
		}
	})

	t.Run("codex stays", func(t *testing.T) {
		run := launchExecProbe(t, sub, "codex", nil)
		if run.moved != "" || strings.Contains(run.out, "starting claude in") {
			t.Errorf("codex launch moved to %q (stderr %q), want it left in the subfolder", run.moved, run.out)
		}
	})

	t.Run("the root itself does not move", func(t *testing.T) {
		run := launchExecProbe(t, root, "claude", nil)
		if run.moved != "" || strings.Contains(run.out, "starting claude in") {
			t.Errorf("launch at the root moved to %q (stderr %q)", run.moved, run.out)
		}
	})
}

func TestConsumeHereFlag(t *testing.T) {
	tests := []struct {
		in       []string
		wantRest []string
		wantHere bool
	}{
		{nil, nil, false},
		{[]string{"--here"}, []string{}, true},
		{[]string{"--here", "-p", "hi"}, []string{"-p", "hi"}, true},
		{[]string{"--", "--here"}, []string{"--", "--here"}, false},
		{[]string{"-p", "--here"}, []string{"-p", "--here"}, false},
	}
	for _, tc := range tests {
		rest, here := consumeHereFlag(tc.in)
		if here != tc.wantHere || !slices.Equal(rest, tc.wantRest) {
			t.Errorf("consumeHereFlag(%q) = %q, %t; want %q, %t", tc.in, rest, here, tc.wantRest, tc.wantHere)
		}
	}
}
