package sweep

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, dir, name, body string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

const briefBody = "# Brief: fix issues\n\n## Where to work\n\n- `acme/widget`: repo `/x/widget`, base worktree `/x/wt`\n- `acme/gadget`: repo `/x/gadget`\n- `acme/../evil`: nope\n\n## Phase 1 - pick\n\n1. List open issues: `gh issue list --repo acme/<repo>`; also see `other/ignored`.\n"

func TestReadBriefsPhases(t *testing.T) {
	root := t.TempDir()
	h := func(n int) time.Time { return t0.Add(time.Duration(-n) * time.Hour) }
	// launched only
	put(t, filepath.Join(root, "cos-launched"), "brief.md", briefBody, h(5))
	// plan: picks complete
	put(t, filepath.Join(root, "cos-plan"), "brief.md", briefBody, h(5))
	put(t, filepath.Join(root, "cos-plan"), "picks.md", "1. a\n\nPICKS-COMPLETE\n", h(4))
	// picks not complete stays launched
	put(t, filepath.Join(root, "cos-partial"), "brief.md", briefBody, h(5))
	put(t, filepath.Join(root, "cos-partial"), "picks.md", "1. a\n", h(4))
	// go
	put(t, filepath.Join(root, "cos-go"), "brief.md", briefBody, h(5))
	put(t, filepath.Join(root, "cos-go"), "picks.md", "PICKS-COMPLETE\n", h(4))
	put(t, filepath.Join(root, "cos-go"), "go.md", "GO\n", h(3))
	// report after go
	put(t, filepath.Join(root, "cos-report"), "brief.md", briefBody, h(5))
	put(t, filepath.Join(root, "cos-report"), "picks.md", "PICKS-COMPLETE\n", h(4))
	put(t, filepath.Join(root, "cos-report"), "go.md", "GO\n", h(3))
	put(t, filepath.Join(root, "cos-report"), "report.md", "done\nREPORT-COMPLETE\n\n", h(2))
	// a report written before the GO is not the finish line
	put(t, filepath.Join(root, "cos-early"), "brief.md", briefBody, h(5))
	put(t, filepath.Join(root, "cos-early"), "picks.md", "PICKS-COMPLETE\n", h(4))
	put(t, filepath.Join(root, "cos-early"), "report.md", "REPORT-COMPLETE\n", h(3))
	put(t, filepath.Join(root, "cos-early"), "go.md", "GO\n", h(2))
	// not a worker directory
	put(t, filepath.Join(root, "other-thing"), "brief.md", briefBody, h(1))
	// older than since
	put(t, filepath.Join(root, "cos-old"), "brief.md", briefBody, t0.Add(-30*24*time.Hour))

	briefs, skipped, err := ReadBriefs(root, "cos-", t0.Add(-24*time.Hour))
	if err != nil || skipped != 0 {
		t.Fatalf("ReadBriefs: skipped=%d err=%v", skipped, err)
	}
	got := map[string]Brief{}
	for _, b := range briefs {
		got[b.Name] = b
	}
	want := map[string]string{"cos-launched": PhaseLaunched, "cos-plan": PhasePlan, "cos-partial": PhaseLaunched, "cos-go": PhaseGo, "cos-report": PhaseReport, "cos-early": PhaseGo}
	if len(got) != len(want) {
		t.Errorf("briefs = %v", slices.Collect(func(yield func(string) bool) {
			for k := range got {
				if !yield(k) {
					return
				}
			}
		}))
	}
	for name, phase := range want {
		if got[name].Phase != phase {
			t.Errorf("%s phase = %q, want %q", name, got[name].Phase, phase)
		}
	}
	r := got["cos-report"]
	if !r.Marks[PhaseReport].Equal(h(2)) || !r.Latest.Equal(h(2)) {
		t.Errorf("report marks = %+v latest %v", r.Marks, r.Latest)
	}
	if !slices.Equal(r.Repos, []string{"acme/gadget", "acme/widget"}) {
		t.Errorf("repos = %v (only the Where to work section, valid names, sorted)", r.Repos)
	}
}

func TestReadBriefsRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	put(t, outside, "report.md", "REPORT-COMPLETE\n", t0)
	put(t, outside, "brief.md", briefBody, t0)

	// a symlinked brief directory is skipped and counted
	if err := os.Symlink(outside, filepath.Join(root, "cos-link")); err != nil {
		t.Fatal(err)
	}
	// a symlinked file inside a real directory is ignored
	realDir := filepath.Join(root, "cos-real")
	put(t, realDir, "brief.md", briefBody, t0.Add(-time.Hour))
	put(t, realDir, "picks.md", "PICKS-COMPLETE\n", t0.Add(-time.Hour))
	put(t, realDir, "go.md", "GO\n", t0.Add(-time.Hour))
	if err := os.Symlink(filepath.Join(outside, "report.md"), filepath.Join(realDir, "report.md")); err != nil {
		t.Fatal(err)
	}
	briefs, skipped, err := ReadBriefs(root, "cos-", t0.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(briefs) != 1 || briefs[0].Name != "cos-real" || briefs[0].Phase != PhaseGo {
		t.Errorf("briefs = %+v: the symlinked report must not count", briefs)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the symlinked directory)", skipped)
	}
}

func TestReadBriefsCapsWhatItReads(t *testing.T) {
	root := t.TempDir()
	huge := briefBody + strings.Repeat("filler line\n", 100_000)
	put(t, filepath.Join(root, "cos-big"), "brief.md", huge, t0)
	briefs, _, err := ReadBriefs(root, "cos-", t0.Add(-time.Hour))
	if err != nil || len(briefs) != 1 || len(briefs[0].Repos) != 2 {
		t.Fatalf("briefs = %+v err = %v", briefs, err)
	}
}

func TestReadBriefsMissingDir(t *testing.T) {
	if _, _, err := ReadBriefs(filepath.Join(t.TempDir(), "nope"), "cos-", t0); err == nil {
		t.Error("a missing briefs directory was not an error")
	}
}

func TestReadFiled(t *testing.T) {
	root := t.TempDir()
	put(t, root, "filed.txt", "https://github.com/acme/widget/issues/1\tSecurity: leak\nhttps://github.com/acme/widget/issues/2\tDocs\nno tab here\n", t0)
	got := ReadFiled(root)
	if !slices.Equal(got, []string{"Security: leak", "Docs"}) {
		t.Errorf("filed = %v", got)
	}
	if got := ReadFiled(t.TempDir()); len(got) != 0 {
		t.Errorf("absent filed.txt gave %v", got)
	}
}
