package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestResolve_SymlinkedMatchFailsClosed pins today's behavior while
// forgectl#1042 waits on a decision: a project match is compared with the
// physical working directory only, so a match written through a symlink
// applies neither through the symlink nor at its target. Every way found of
// letting it apply let content a repository can commit (a symlink) steer the
// posture; see the comments on #1042.
func TestResolve_SymlinkedMatchFailsClosed(t *testing.T) {
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
	allow := true
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AllowDanger: new(bool)},
		Projects: []config.LaunchProject{{Match: link, AllowDanger: &allow}},
	}
	for _, cwd := range []string{link, filepath.Join(link, "sub"), dir, filepath.Join(dir, "sub")} {
		p, proj, err := resolveMatched(lc, cwd, os.UserHomeDir)
		if err != nil {
			t.Fatalf("%s: %v", cwd, err)
		}
		if proj != nil || p.AllowDanger || p.Match != "" {
			t.Errorf("%s: the symlinked block applied (allow_danger %v, match %q)", cwd, p.AllowDanger, p.Match)
		}
	}
}
