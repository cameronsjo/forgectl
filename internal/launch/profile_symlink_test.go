package launch

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestResolve_SymlinkedMatchApplies pins the fix for forgectl#1042: a project
// match written through a symlink resolves to its target, so it applies both
// through the symlink and at its target instead of falling through to
// [launch.defaults].
func TestResolve_SymlinkedMatchApplies(t *testing.T) {
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
		if proj == nil || !p.AllowDanger || p.Match != link {
			t.Errorf("%s: the symlinked block did not apply (allow_danger %v, match %q)", cwd, p.AllowDanger, p.Match)
		}
	}
}

// TestResolve_UnresolvableMatchWarns pins the best-effort half: a match path
// that cannot be resolved warns and is matched lexically, rather than falling
// through to [launch.defaults] without a word.
func TestResolve_UnresolvableMatchWarns(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allow := true
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AllowDanger: new(bool)},
		Projects: []config.LaunchProject{{Match: filepath.Join(base, "does-not-exist"), AllowDanger: &allow}},
	}
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	p, proj, err := resolveMatched(lc, base, os.UserHomeDir)
	if err != nil {
		t.Fatalf("resolveMatched: %v", err)
	}
	if proj != nil || p.AllowDanger || p.Match != "" {
		t.Errorf("unresolvable block applied (allow_danger %v, match %q)", p.AllowDanger, p.Match)
	}
	if !strings.Contains(buf.String(), "cannot be resolved") {
		t.Errorf("no warning for unresolvable match; log = %q", buf.String())
	}
}
