package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestResolve_SymlinkedMatch is forgectl#1042: a project block whose match is
// written through a symlink applies to its directory however the cwd spells
// the path, and the profile reports the match as written.
//
// Mutation: drop withResolvedMatches from resolveMatched and the physical-cwd
// case falls back to the defaults.
func TestResolve_SymlinkedMatch(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "dir")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	lc := config.LaunchConfig{Projects: []config.LaunchProject{{Match: link, PermissionMode: "acceptEdits"}}}

	for _, cwd := range []string{dir, filepath.Join(dir, "sub"), link, filepath.Join(link, "sub")} {
		p, proj, err := resolveMatched(lc, cwd, os.UserHomeDir)
		if err != nil {
			t.Fatalf("%s: %v", cwd, err)
		}
		if p.PermissionMode != "acceptEdits" || p.Match != link || proj == nil || proj.Match != link {
			t.Errorf("%s: permission %q, match %q, block %v; want acceptEdits from the block matched as %q", cwd, p.PermissionMode, p.Match, proj, link)
		}
	}
	if lc.Projects[0].Match != link {
		t.Errorf("the config was changed: match %q", lc.Projects[0].Match)
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
