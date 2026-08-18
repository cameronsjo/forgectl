package projects

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The resolver decides which directory a session runs in, so every test here
// is really asking the same question: can it be made to pick one the operator
// did not name?

// projectRoot builds a root with both layouts forgectl creates.
func projectRoot(t *testing.T) *Client {
	t.Helper()
	root := t.TempDir()

	mkdir := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
		return p
	}

	mkdir("flatproject")                            // flat layout
	mkdir("github.com", "cameronsjo", "forgectl")   // host/owner/repo
	mkdir("github.com", "cameronsjo", "cadence")    //
	mkdir("git.example", "someone", "forgectl")     // same repo name, other host
	mkdir("github.com", "cameronsjo", "deep", "no") // too deep to match

	return &Client{Dir: root}
}

// TestResolveTarget_FindsABareNameInEitherLayout is the baseline. Without it
// every refusal below could pass against a resolver that finds nothing.
func TestResolveTarget_FindsABareNameInEitherLayout(t *testing.T) {
	c := projectRoot(t)

	flat, err := c.ResolveTarget("flatproject")
	if err != nil {
		t.Fatalf("a flat project was not found: %v", err)
	}
	if filepath.Base(flat) != "flatproject" {
		t.Errorf("resolved to %q, want the flat project", flat)
	}

	nested, err := c.ResolveTarget("cadence")
	if err != nil {
		t.Fatalf("a host/owner/repo project was not found: %v", err)
	}
	if !strings.HasSuffix(nested, filepath.Join("github.com", "cameronsjo", "cadence")) {
		t.Errorf("resolved to %q, want the nested project", nested)
	}
}

// TestResolveTarget_RefusesAnAmbiguousName is the rule that matters most. Two
// projects share a name across hosts, and picking either would be picking
// which of the operator's repositories to open a session in.
func TestResolveTarget_RefusesAnAmbiguousName(t *testing.T) {
	c := projectRoot(t)

	if _, err := c.ResolveTarget("forgectl"); !errors.Is(err, ErrTargetAmbiguous) {
		t.Errorf("a name matching two projects = %v, want ErrTargetAmbiguous", err)
	}
}

// TestResolveTarget_MatchesExactly keeps prefix and case from being a match.
// A resolver that guessed would run a session somewhere the operator did not
// name, and would look like it worked.
//
// The "CADENCE" row is the one that earns its place. macOS's default
// filesystem is case-insensitive, so a stat-based lookup finds "cadence" when
// asked for "CADENCE" — and FileInfo.Name() reports the spelling that was
// asked for, so nothing downstream notices. This row fails on a Mac against
// any implementation that joins a path and stats it, and passes trivially on
// Linux, which is exactly why it has to be here rather than assumed.
func TestResolveTarget_MatchesExactly(t *testing.T) {
	c := projectRoot(t)

	for _, name := range []string{"caden", "cadence2", "CADENCE", "cadence "} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.ResolveTarget(name); !errors.Is(err, ErrTargetNotFound) {
				t.Errorf("ResolveTarget(%q) = %v, want ErrTargetNotFound", name, err)
			}
		})
	}
}

// TestResolveTarget_DoesNotWalkBelowTheKnownLayouts keeps a vendored checkout
// from being a match. Matching one would start a session inside a dependency.
func TestResolveTarget_DoesNotWalkBelowTheKnownLayouts(t *testing.T) {
	c := projectRoot(t)

	// github.com/cameronsjo/deep/no exists, one level below where the
	// host/owner/repo layout ends.
	if _, err := c.ResolveTarget("no"); !errors.Is(err, ErrTargetNotFound) {
		t.Errorf("a directory below the known layouts = %v, want ErrTargetNotFound", err)
	}
	// The control: its parent, which IS at repo depth, does match.
	if _, err := c.ResolveTarget("deep"); err != nil {
		t.Errorf("a directory at repo depth was not found: %v", err)
	}
}

// TestResolveTarget_DoesNotFollowDirectorySymlinksByName is the redirection
// guard.
//
// A symlink inside the root could otherwise silently send a launch anywhere on
// the filesystem while the operator typed nothing but a project name. Naming
// the target explicitly is the supported way to reach a linked directory, and
// the second half asserts that path still works — otherwise this test would be
// consistent with a resolver that simply refuses symlinks everywhere.
func TestResolveTarget_DoesNotFollowDirectorySymlinksByName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	c := projectRoot(t)

	elsewhere := t.TempDir()
	link := filepath.Join(c.Dir, "linked")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}

	if _, err := c.ResolveTarget("linked"); !errors.Is(err, ErrTargetNotFound) {
		t.Errorf("a bare name resolved through a directory symlink = %v, want ErrTargetNotFound", err)
	}

	// Named explicitly, it resolves — to the link's target, canonicalized.
	got, err := c.ResolveTarget(link)
	if err != nil {
		t.Fatalf("an explicitly named symlink was refused: %v", err)
	}
	want, err := filepath.EvalSymlinks(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("explicit path resolved to %q, want the canonical target %q", got, want)
	}
}

// TestResolveTarget_TreatsPathsAsLocations covers the other shape. An explicit
// path may be anywhere, including outside the root, because naming it is the
// operator making the choice themselves.
func TestResolveTarget_TreatsPathsAsLocations(t *testing.T) {
	c := projectRoot(t)
	outside := t.TempDir()

	got, err := c.ResolveTarget(outside)
	if err != nil {
		t.Fatalf("an absolute path outside the root was refused: %v", err)
	}
	want, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("resolved to %q, want %q", got, want)
	}

	// A relative path is a location too, and is resolved against the caller's
	// cwd rather than the project root.
	if err := os.Chdir(outside); err != nil {
		t.Skipf("cannot change directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(c.Dir) })

	if _, err := c.ResolveTarget("."); err != nil {
		t.Errorf("the current directory was refused: %v", err)
	}
}

// TestResolveTarget_RefusesNonDirectories keeps a file from becoming a working
// directory, where it would fail later and less clearly.
func TestResolveTarget_RefusesNonDirectories(t *testing.T) {
	c := projectRoot(t)

	file := filepath.Join(c.Dir, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := c.ResolveTarget(file); !errors.Is(err, ErrTargetUnusable) {
		t.Errorf("a regular file as an explicit path = %v, want ErrTargetUnusable", err)
	}
	if _, err := c.ResolveTarget("afile"); !errors.Is(err, ErrTargetNotFound) {
		t.Errorf("a regular file as a bare name = %v, want ErrTargetNotFound", err)
	}
	if _, err := c.ResolveTarget(""); !errors.Is(err, ErrTargetUnusable) {
		t.Errorf("an empty target = %v, want ErrTargetUnusable", err)
	}
}

// TestResolveTarget_RefusesAnUnsearchablyLargeRoot proves the cap binds.
//
// The root is operator-controlled and can be pathological. A resolver that
// called ReadDir(-1) would materialize the whole thing before anything could
// refuse; this asserts the budget is reached and reported rather than paged in.
func TestResolveTarget_RefusesAnUnsearchablyLargeRoot(t *testing.T) {
	root := t.TempDir()
	// One over the budget, so the cap is what stops the walk.
	for i := range maxExaminedEntries + 1 {
		if err := os.Mkdir(filepath.Join(root, "d"+itoa(i)), 0o750); err != nil {
			t.Skipf("cannot create the fixture (filesystem limit?): %v", err)
		}
	}
	c := &Client{Dir: root}

	if _, err := c.ResolveTarget("nothing-here"); !errors.Is(err, ErrSearchTooLarge) {
		t.Errorf("an oversized root = %v, want ErrSearchTooLarge", err)
	}
}

// TestResolveTarget_ErrorsNeverEchoTheName keeps a command-line argument out of
// a terminal-rendered error. It is not secret, but it is attacker-shaped input
// to a message that gets printed.
func TestResolveTarget_ErrorsNeverEchoTheName(t *testing.T) {
	c := projectRoot(t)

	hostile := "\x1b[2Kmissing-project"
	_, err := c.ResolveTarget(hostile)
	if err == nil {
		t.Fatal("a missing project resolved successfully")
	}
	if strings.Contains(err.Error(), hostile) || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("the error echoed the operator's argument: %q", err.Error())
	}
}

// itoa avoids importing strconv for one call in a fixture loop.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
