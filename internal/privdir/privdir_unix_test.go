//go:build unix

package privdir_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// spec builds a create-mode spec under a fresh base.
func spec(base string) privdir.Spec {
	return privdir.Spec{
		Base:         base,
		Leaf:         "forgectl",
		Mode:         0o700,
		AncestorMode: 0o755,
		Create:       true,
	}
}

// pin runs Pin and registers the descriptor for close.
func pin(t *testing.T, s privdir.Spec) (int, error) {
	t.Helper()
	fd, err := privdir.Pin(s)
	if err == nil {
		t.Cleanup(func() { _ = unix.Close(fd) })
	}
	return fd, err
}

// scratch returns a short base path. It is deliberately not t.TempDir(): that
// embeds the test name, and callers of this package bind Unix sockets under
// the pinned directory, where macOS caps the whole path near 104 bytes.
func scratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "state")
}

// TestPin_CreatesThePrivateLeafAndReturnsAUsableDescriptor is the happy path,
// and it asserts the descriptor is usable *descriptor-relative* — which is the
// entire reason this package returns an fd instead of a path.
func TestPin_CreatesThePrivateLeafAndReturnsAUsableDescriptor(t *testing.T) {
	base := scratch(t)

	fd, err := pin(t, spec(base))
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}

	leaf := filepath.Join(base, "forgectl")
	info, err := os.Lstat(leaf)
	if err != nil {
		t.Fatalf("lstat leaf: %v", err)
	}
	if !info.IsDir() {
		t.Error("leaf is not a directory")
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("leaf mode = %o, want 700", got)
	}

	// The descriptor must actually work for openat, or the pin bought nothing.
	child, err := unix.Openat(fd, "data", unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatalf("openat against the pinned descriptor: %v", err)
	}
	_ = unix.Close(child)
	if _, err := os.Lstat(filepath.Join(leaf, "data")); err != nil {
		t.Errorf("the file created through the descriptor is not in the leaf: %v", err)
	}
}

// TestPin_AbsentIsDistinctFromUnsafe is the distinction the whole error surface
// exists for. A caller that treats "never created" as an empty store must not
// treat "exists and was refused" the same way — collapsing them turns a
// security refusal into a silent empty result.
func TestPin_AbsentIsDistinctFromUnsafe(t *testing.T) {
	base := scratch(t)

	readOnly := spec(base)
	readOnly.Create = false
	if _, err := pin(t, readOnly); !errors.Is(err, privdir.ErrAbsent) {
		t.Fatalf("Pin on a missing base = %v, want ErrAbsent", err)
	}

	// A base that exists with no leaf is still absent, not unsafe.
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := pin(t, readOnly); !errors.Is(err, privdir.ErrAbsent) {
		t.Fatalf("Pin with no leaf = %v, want ErrAbsent", err)
	}

	// And neither of those wrote anything.
	if _, err := os.Lstat(filepath.Join(base, "forgectl")); !os.IsNotExist(err) {
		t.Error("a read-only Pin created the leaf")
	}

	// The control: with Create the same spec succeeds, so the refusals above
	// are the absence being reported rather than Pin failing generally.
	if _, err := pin(t, spec(base)); err != nil {
		t.Errorf("Pin with Create = %v, want success", err)
	}
}

// TestPin_RefusesASubstitutedLeaf is the attack the package is named for: a
// symlink standing where the private directory belongs, pointing at something
// the attacker wants written to.
func TestPin_RefusesASubstitutedLeaf(t *testing.T) {
	base := scratch(t)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}

	elsewhere := t.TempDir()
	// Pinned explicitly rather than inherited from t.TempDir(), whose mode is
	// 0o777 masked by the ambient umask — under umask 077 the fixture would
	// already be 0o700 and the assertion below would fail spuriously.
	//nolint:gosec // G301: the fixture stands in for an attacker's directory
	if err := os.Chmod(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(elsewhere, "victim")
	if err := os.WriteFile(victim, []byte("do not touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(base, "forgectl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := pin(t, spec(base)); !errors.Is(err, privdir.ErrUnsafe) {
		t.Fatalf("Pin through a symlinked leaf = %v, want ErrUnsafe", err)
	}

	// The mode assertion is the one doing the work: a repair that landed on
	// the symlink's target would narrow it to the private mode.
	info, err := os.Lstat(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("the attacker's directory mode = %o, want 755 — the refusal chmod'd it", got)
	}

	// The content check cannot fail today, because no path in Pin writes a
	// file under the leaf. It is kept as a tripwire for the change that would
	// add one before the identity check rather than after.
	//nolint:gosec // G304: the path is this test's own fixture
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "do not touch\n" {
		t.Errorf("the refusal disturbed the attacker's target: %q, %v", got, err)
	}
}

// TestPin_ReadOnlyOpensAnExistingLeaf covers the success path a reader takes:
// Create is false, the leaf is already there, and every check still runs.
func TestPin_ReadOnlyOpensAnExistingLeaf(t *testing.T) {
	base := scratch(t)
	if _, err := pin(t, spec(base)); err != nil {
		t.Fatalf("seeding Pin: %v", err)
	}

	readOnly := spec(base)
	readOnly.Create = false
	fd, err := pin(t, readOnly)
	if err != nil {
		t.Fatalf("read-only Pin on an existing leaf = %v, want success", err)
	}

	// The descriptor is the same directory, and usable.
	child, err := unix.Openat(fd, "probe", unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatalf("openat against the read-only pin: %v", err)
	}
	_ = unix.Close(child)
	if _, err := os.Lstat(filepath.Join(base, "forgectl", "probe")); err != nil {
		t.Errorf("the read-only pin points somewhere else: %v", err)
	}
}

// TestPin_RefusesANonDirectoryLeaf covers a plain file where the directory
// belongs — the other shape a hostile or merely broken leaf takes.
func TestPin_RefusesANonDirectoryLeaf(t *testing.T) {
	base := scratch(t)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "forgectl"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := pin(t, spec(base)); !errors.Is(err, privdir.ErrUnsafe) {
		t.Fatalf("Pin with a file at the leaf = %v, want ErrUnsafe", err)
	}
}

// TestPin_NarrowsABroadLeaf proves the mode repair runs, and that it lands on
// the leaf rather than anything else. The ordering it depends on — identity
// and ownership proven *before* the chmod — is what keeps a repair from ever
// touching an object that was never ours.
func TestPin_NarrowsABroadLeaf(t *testing.T) {
	base := scratch(t)
	leaf := filepath.Join(base, "forgectl")
	// A deliberately over-broad leaf is the input under test — Pin must narrow
	// it. Creating it at a mode gosec would prefer would test nothing.
	//nolint:gosec // G301: the broad mode is the fixture
	if err := os.MkdirAll(leaf, 0o777); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G302: ditto — the mode is what Pin is asked to repair
	if err := os.Chmod(leaf, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := pin(t, spec(base)); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	info, err := os.Lstat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("leaf mode = %o after Pin, want 700", got)
	}
}

// TestPin_LeavesAncestorsConventional pins the split between the two modes.
// A base several levels below a shared root should not make that root private
// forever — enabling one feature must not be what decides ~/.local is 0700.
func TestPin_LeavesAncestorsConventional(t *testing.T) {
	root, err := os.MkdirTemp("", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	base := filepath.Join(root, "share", "deep", "state")
	if _, err := pin(t, spec(base)); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	for _, dir := range []string{
		filepath.Join(root, "share"),
		filepath.Join(root, "share", "deep"),
	} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("lstat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Errorf("ancestor %s mode = %o, want 755", dir, got)
		}
	}

	// The base and the leaf are the private ones.
	for _, dir := range []string{base, filepath.Join(base, "forgectl")} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("lstat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, got)
		}
	}
}

// TestPin_RefusesASpecItCannotCheck covers the inputs that would quietly
// weaken the guarantee. A multi-segment leaf is the one that matters: it puts
// intermediate path components back into the resolution, which is exactly what
// pinning a descriptor exists to remove.
func TestPin_RefusesASpecItCannotCheck(t *testing.T) {
	base := scratch(t)

	tests := map[string]privdir.Spec{
		"no base":            {Leaf: "forgectl", Mode: 0o700, AncestorMode: 0o755, Create: true},
		"no leaf":            {Base: base, Mode: 0o700, AncestorMode: 0o755, Create: true},
		"multi-segment leaf": {Base: base, Leaf: "a/b", Mode: 0o700, AncestorMode: 0o755, Create: true},
		"traversing leaf":    {Base: base, Leaf: "..", Mode: 0o700, AncestorMode: 0o755, Create: true},
		"current-dir leaf":   {Base: base, Leaf: ".", Mode: 0o700, AncestorMode: 0o755, Create: true},
		"absolute leaf":      {Base: base, Leaf: "/etc", Mode: 0o700, AncestorMode: 0o755, Create: true},
		// The separator alone is the one absolute path filepath.Base returns
		// unchanged, so a round-trip check through Base admits it. POSIX
		// ignores the dirfd when the path is absolute, so openat would hand
		// back a descriptor on the root filesystem — and the mode repair would
		// then chmod / to 0700 for any caller running as root.
		"the root directory":      {Base: base, Leaf: "/", Mode: 0o700, AncestorMode: 0o755, Create: true},
		"double separator":        {Base: base, Leaf: "//", Mode: 0o700, AncestorMode: 0o755, Create: true},
		"leaf with a NUL":         {Base: base, Leaf: "a\x00b", Mode: 0o700, AncestorMode: 0o755, Create: true},
		"leaf with a setgid mode": {Base: base, Leaf: "forgectl", Mode: 0o2700, AncestorMode: 0o755, Create: true},
		"no mode":                 {Base: base, Leaf: "forgectl", AncestorMode: 0o755, Create: true},
		"no ancestor mode":        {Base: base, Leaf: "forgectl", Mode: 0o700, Create: true},
		"the zero value spec":     {},
	}

	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := pin(t, s); !errors.Is(err, privdir.ErrUnsafe) {
				t.Errorf("Pin(%s) = %v, want ErrUnsafe", name, err)
			}
			// A refused spec must not have created anything.
			if _, err := os.Lstat(base); !os.IsNotExist(err) {
				t.Errorf("Pin(%s) created the base despite refusing", name)
			}
		})
	}
}

// TestPin_RefusalsAreTerminalSafe keeps a hostile path out of the operator's
// terminal. Callers print these refusals.
func TestPin_RefusalsAreTerminalSafe(t *testing.T) {
	root, err := os.MkdirTemp("", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// A base that is a regular file produces a refusal quoting the OS error,
	// which is the path most likely to carry attacker-influenced text.
	base := filepath.Join(root, "state\x1b[31m")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Skipf("cannot create a control-character path here: %v", err)
	}

	s := spec(base)
	s.Create = false
	_, err = pin(t, s)
	if err == nil {
		t.Fatal("Pin accepted a file as the base")
	}
	if got := err.Error(); got != "" && containsEscape(got) {
		t.Errorf("refusal writes a raw escape to the terminal: %q", got)
	}
}

func containsEscape(s string) bool {
	for i := range len(s) {
		if s[i] == 0x1b {
			return true
		}
	}
	return false
}
