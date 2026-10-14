package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// symlinkTree makes base/dir/sub and base/link -> base/dir, with base
// resolved so the temp dir's own symlinks (macOS /var) do not interfere.
func symlinkTree(t *testing.T) (base, dir, link string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(base, "dir")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return base, dir, link
}

// TestResolve_SymlinkedMatch is forgectl#1042: a project block whose match is
// written through a symlink applies when the cwd is spelled through that
// symlink, and the profile reports the match as written.
//
// Mutation: drop spelledCWD from resolveMatched and the block stops applying.
func TestResolve_SymlinkedMatch(t *testing.T) {
	_, _, link := symlinkTree(t)
	lc := config.LaunchConfig{Projects: []config.LaunchProject{{Match: link, PermissionMode: "acceptEdits"}}}
	for _, cwd := range []string{link, filepath.Join(link, "sub")} {
		p, proj, err := resolveMatched(lc, cwd, os.UserHomeDir)
		if err != nil {
			t.Fatalf("%s: %v", cwd, err)
		}
		if p.PermissionMode != "acceptEdits" || p.Match != link || proj == nil || proj.Match != link {
			t.Errorf("%s: permission %q, match %q, block %v; want acceptEdits from the block matched as %q", cwd, p.PermissionMode, p.Match, proj, link)
		}
	}
}

// TestResolve_SymlinkedMatchNeverWidens is the review finding on #1262: a
// match is never resolved, so a committed `scratch -> ..` does not make a
// block written for scratch apply to its parent or a sibling.
//
// Mutation: resolve the match with filepath.EvalSymlinks and the parent and
// sibling get the looser block.
func TestResolve_SymlinkedMatchNeverWidens(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foo := filepath.Join(base, "foo")
	sibling := filepath.Join(foo, "other")
	if err := os.MkdirAll(sibling, 0o750); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(foo, "scratch")
	if err := os.Symlink("..", scratch); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	allow := true
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AllowDanger: new(bool)},
		Projects: []config.LaunchProject{{Match: scratch, AllowDanger: &allow}},
	}
	for _, cwd := range []string{foo, sibling, base} {
		p, proj, err := resolveMatched(lc, cwd, os.UserHomeDir)
		if err != nil {
			t.Fatalf("%s: %v", cwd, err)
		}
		if proj != nil || p.AllowDanger || p.Match != "" {
			t.Errorf("%s: got the scratch block (allow_danger %v, match %q)", cwd, p.AllowDanger, p.Match)
		}
	}
}

// TestResolve_UnresolvableMatchKeepsItsSpelling: a match naming a directory
// that does not exist yet still matches by its written path.
func TestResolve_UnresolvableMatchKeepsItsSpelling(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-yet")
	lc := config.LaunchConfig{Projects: []config.LaunchProject{{Match: missing, Model: "sonnet"}}}
	p, err := resolveWithHome(lc, filepath.Join(missing, "x"), os.UserHomeDir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Match != missing || p.Model != "sonnet" {
		t.Errorf("match %q model %q; want the block to apply", p.Match, p.Model)
	}
}
