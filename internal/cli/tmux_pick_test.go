package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// seshPickClient builds a client whose sesh PATH check is stubbed, so a call
// that gets past forgectl's own gate is recorded on the fake runner instead of
// needing a real sesh binary.
func seshPickClient(fake *exec.FakeRunner) *tmux.Client {
	return tmux.New(fake,
		tmux.WithInsideTmux(func() bool { return true }),
		tmux.WithLookPath(func(string) (string, error) { return "sesh", nil }),
	)
}

// seshCalls counts the recorded invocations of sesh.
func seshCalls(fake *exec.FakeRunner) int {
	n := 0
	for _, c := range fake.Calls {
		if c.Name == "sesh" {
			n++
		}
	}
	return n
}

// hostileSeshNames are candidates sesh would hand to `tmux new-session -c`
// unescaped (#841): a directory path and a zoxide-listed path carrying a
// tmux #() job, plus a bare '#' format.
var hostileSeshNames = []string{
	"/tmp/x#(touch marker)",
	"~/src/proj#(id)",
	"#{pane_pid}",
}

func TestSeshPick_RefusesHashAtBothEntryPoints(t *testing.T) {
	ctx := context.Background()
	entryPoints := []struct {
		name string
		call func(*tmux.Client, string) error
	}{
		{"tmux pick <name>", func(c *tmux.Client, name string) error {
			cmd := newTmuxPickCmd(c)
			cmd.SetArgs([]string{"--", name})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			return cmd.ExecuteContext(ctx)
		}},
		{"TUI picker dispatch", func(c *tmux.Client, name string) error {
			return dispatchAction(ctx, c, tui.Action{Kind: tui.ActionPick, Pick: name})
		}},
	}
	for _, ep := range entryPoints {
		for _, name := range hostileSeshNames {
			t.Run(ep.name+"/"+name, func(t *testing.T) {
				fake := liveServer()
				err := ep.call(seshPickClient(fake), name)
				if !errors.Is(err, errSeshUnsafeCandidate) {
					t.Fatalf("err = %v, want errSeshUnsafeCandidate", err)
				}
				if n := seshCalls(fake); n != 0 {
					t.Fatalf("sesh was invoked %d time(s) for %q; the gate must refuse before sesh runs", n, name)
				}
			})
		}
	}
}

func TestSeshPick_PassesOrdinaryNameToSesh(t *testing.T) {
	fake := liveServer()
	if err := seshPick(context.Background(), seshPickClient(fake), "/tmp/project"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := seshCalls(fake); n != 1 {
		t.Fatalf("sesh calls = %d, want 1", n)
	}
	last := fake.Last()
	want := []string{"connect", "--", "/tmp/project"}
	if len(last.Args) != len(want) {
		t.Fatalf("args = %q, want %q", last.Args, want)
	}
	for i := range want {
		if last.Args[i] != want[i] {
			t.Fatalf("args = %q, want %q", last.Args, want)
		}
	}
}

// symlinkOrSkip creates link -> target, skipping where the platform refuses
// symlinks (unprivileged Windows).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

// A '#'-free candidate that is a symlink to a '#' directory: sesh's namer
// resolves it with EvalSymlinks and names the session after the target, so
// the '#' reaches tmux although the candidate has none.
func TestSeshPick_RefusesSymlinkToHashDir(t *testing.T) {
	root := t.TempDir()
	hostile := filepath.Join(root, "x#(id)")
	if err := os.Mkdir(hostile, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent")
	symlinkOrSkip(t, hostile, link)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	symlinkOrSkip(t, hostile, filepath.Join(home, "proj"))

	for _, name := range []string{link, "~/proj", "$HOME/proj"} {
		t.Run(name, func(t *testing.T) {
			fake := liveServer()
			err := seshPick(context.Background(), seshPickClient(fake), name)
			if !errors.Is(err, errSeshUnsafeCandidate) {
				t.Fatalf("err = %v, want errSeshUnsafeCandidate", err)
			}
			if n := seshCalls(fake); n != 0 {
				t.Fatalf("sesh was invoked %d time(s); the gate must refuse before sesh runs", n)
			}
		})
	}
}

// sesh makes a candidate absolute against the working directory it inherits
// from forgectl, so a relative candidate naming a '#' cwd reaches
// `new-session -c` with the '#' although the candidate has none. "missing"
// does not exist, so EvalSymlinks fails and only the Abs path can catch it.
func TestSeshPick_RefusesRelativeCandidateInHashCwd(t *testing.T) {
	hostile := filepath.Join(t.TempDir(), "x#(id)")
	if err := os.MkdirAll(filepath.Join(hostile, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(hostile)

	for _, name := range []string{".", "./", "sub/..", "missing"} {
		t.Run(name, func(t *testing.T) {
			fake := liveServer()
			err := seshPick(context.Background(), seshPickClient(fake), name)
			if !errors.Is(err, errSeshUnsafeCandidate) {
				t.Fatalf("err = %v, want errSeshUnsafeCandidate", err)
			}
			if n := seshCalls(fake); n != 0 {
				t.Fatalf("sesh was invoked %d time(s); the gate must refuse before sesh runs", n)
			}
		})
	}
}

// The resolution must not over-refuse: a symlink to a clean directory still
// reaches sesh, under the name the user picked.
func TestSeshPick_PassesSymlinkToCleanDir(t *testing.T) {
	root := t.TempDir()
	clean := filepath.Join(root, "project")
	if err := os.Mkdir(clean, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	symlinkOrSkip(t, clean, link)

	fake := liveServer()
	if err := seshPick(context.Background(), seshPickClient(fake), link); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := seshCalls(fake); n != 1 {
		t.Fatalf("sesh calls = %d, want 1", n)
	}
	if got := fake.Last().Args; got[len(got)-1] != link {
		t.Fatalf("sesh got %q, want the picked name %q", got, link)
	}
}

// The refusal echoes a name nobody at the terminal chose, so its length is
// capped rather than printed whole.
func TestSeshPick_RefusalEchoIsCapped(t *testing.T) {
	name := "/tmp/x#(" + strings.Repeat("a", 20000) + ")"
	err := seshPick(context.Background(), seshPickClient(liveServer()), name)
	if !errors.Is(err, errSeshUnsafeCandidate) {
		t.Fatalf("err = %v, want errSeshUnsafeCandidate", err)
	}
	if n := len(err.Error()); n > 4096 {
		t.Fatalf("refusal message is %d bytes; the echoed name must be capped", n)
	}
}

// TestSeshResolvedPaths_HomeLookup pins forgectl#972: home is looked up only
// for a "~" candidate, and a failed lookup there is an error, not a skipped
// '#' check.
//
// Mutation that turns it red: make seshResolvedPaths return (nil, nil) on the
// lookup error, or look the home directory up unconditionally.
func TestSeshResolvedPaths_HomeLookup(t *testing.T) {
	failing := func() (string, error) { return "", errors.New("no $HOME") }
	if _, err := seshResolvedPaths("~/proj", failing); err == nil {
		t.Fatal("a ~ candidate with no home directory must be an error")
	}
	paths, err := seshResolvedPaths("/tmp/a#b", failing)
	if err != nil || len(paths) == 0 || !strings.Contains(paths[0], "#") {
		t.Fatalf("non-~ candidate must resolve without home: paths=%v err=%v", paths, err)
	}
	ok := func() (string, error) { return "/home/u#x", nil }
	paths, err = seshResolvedPaths("~/proj", ok)
	if err != nil || len(paths) == 0 || paths[0] != "/home/u#x/proj" {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
}

// TestSeshResolvedPaths_DeletedCwdStillChecksExpansion pins the round-2 fix for
// forgectl#972: when filepath.Abs fails (cwd deleted), the env-expanded path is
// still returned for the '#' check instead of skipping it.
//
// Mutation that turns it red: return nil, nil from the filepath.Abs error
// branch in seshResolvedPaths.
func TestSeshResolvedPaths_DeletedCwdStillChecksExpansion(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Skipf("cannot delete the cwd on this platform: %v", err)
	}
	if _, err := filepath.Abs("x"); err == nil {
		t.Skip("filepath.Abs still succeeds with a deleted cwd here")
	}
	t.Setenv("PROBE972", "a#b")
	ok := func() (string, error) { return "/home/u", nil }
	paths, err := seshResolvedPaths("$PROBE972/x", ok)
	if err != nil || len(paths) != 1 || !strings.Contains(paths[0], "#") {
		t.Fatalf("paths=%v err=%v, want the expanded path with '#'", paths, err)
	}
	fake := liveServer()
	if err := seshPick(context.Background(), seshPickClient(fake), "$PROBE972/x"); !errors.Is(err, errSeshUnsafeCandidate) {
		t.Fatalf("seshPick err = %v, want errSeshUnsafeCandidate", err)
	}
	if paths, err := seshResolvedPaths("foo", ok); err != nil || len(paths) != 1 || paths[0] != "foo" {
		t.Fatalf("plain name: paths=%v err=%v", paths, err)
	}
}
