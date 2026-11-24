package ghsource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/workerwatch"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func TestParsePR(t *testing.T) {
	body := func(reviews ...string) string {
		var nodes []string
		for _, r := range reviews {
			nodes = append(nodes, fmt.Sprintf(`{"body":%q,"author":{"login":"octo"}}`, r))
		}
		return `{"data":{"repository":{"pullRequests":{"nodes":[{"number":12,"state":"OPEN","isDraft":true,"headRepository":{"nameWithOwner":"o/r"},"headRefOid":"` + sha +
			`","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING"}}}]},"reviews":{"nodes":[` + strings.Join(nodes, ",") + `]}}]}}}}`
	}
	p, err := parsePR("o/r", "octo", body(
		"<!-- cadence-review: pr-reviewer head="+sha+" crit=1 imp=2 -->\nolder",
		"<!-- cadence-review: security-reviewer head=ffffffffffffffffffffffffffffffffffffffff crit=0 imp=0 -->\nnewest, stale head",
		"plain comment",
	))
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 12 || p.State != "OPEN" || !p.Draft || p.Head != sha || p.Checks != "PENDING" {
		t.Errorf("PR = %+v", p)
	}
	if len(p.Markers) != 2 || p.Markers[0].Reviewer != "security-reviewer" || p.Markers[0].AtHead || !p.Markers[1].AtHead || p.Markers[1].Crit != 1 {
		t.Errorf("markers = %+v, want both, newest first: security-reviewer stale, then pr-reviewer crit=1 at head", p.Markers)
	}

	p, _ = parsePR("o/r", "octo", body("<!-- cadence-review: pr-reviewer head="+sha+" crit=0 imp=3 -->"))
	if len(p.Markers) != 1 || !p.Markers[0].AtHead || p.Markers[0].Imp != 3 {
		t.Errorf("marker at head = %+v", p.Markers)
	}

	if _, err := parsePR("o/r", "octo", `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`); !errors.Is(err, workerwatch.ErrNoPR) {
		t.Errorf("no nodes: err = %v, want workerwatch.ErrNoPR", err)
	}
	if _, err := parsePR("o/r", "octo", `{"errors":[{"message":"x"}]}`); err == nil || errors.Is(err, workerwatch.ErrNoPR) {
		t.Errorf("errors answer: err = %v, want an unusable-answer error", err)
	}
	bad, _ := parsePR("o/r", "octo", strings.Replace(strings.Replace(body(), "PENDING", "WEIRD", 1), sha, "not-a-sha", 1))
	if bad.Checks != "" || bad.Head != "" {
		t.Errorf("unknown enum or bad sha kept: %+v", bad)
	}
}

func TestRepoFromRemote(t *testing.T) {
	tests := map[string]string{
		"git@github.com:o/r.git":         "o/r",
		"https://github.com/o/r":         "o/r",
		"https://gitlab.com/o/r":         "",
		"https://tok@github.com/o/r.git": "o/r",
	}
	for in, want := range tests {
		got, _ := RepoFromRemote(in)
		if got != want {
			t.Errorf("RepoFromRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePRSkipsForksAndOtherAuthors(t *testing.T) {
	answer := `{"data":{"repository":{"pullRequests":{"nodes":[
		{"number":5,"state":"OPEN","headRepository":{"nameWithOwner":"o/r"},"headRefOid":"` + sha + `",
		 "reviews":{"nodes":[
			{"body":"<!-- cadence-review: pr-reviewer head=` + sha + ` crit=2 imp=0 -->","author":{"login":"OCTO"}},
			{"body":"<!-- cadence-review: pr-reviewer head=` + sha + ` crit=0 imp=0 -->","author":{"login":"outsider"}},
			{"body":"<!-- cadence-review: pr-reviewer head=` + sha + ` crit=0 imp=0 -->","author":null}]}},
		{"number":9,"state":"OPEN","headRepository":{"nameWithOwner":"fork/r"},"headRefOid":"` + sha + `"},
		{"number":10,"state":"OPEN","headRepository":null,"headRefOid":"` + sha + `"}]}}}}`
	p, err := parsePR("o/r", "octo", answer)
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 5 {
		t.Errorf("PR = #%d, want #5 (the newer #9 is a fork's, #10 has no head repo)", p.Number)
	}
	if len(p.Markers) != 1 || p.Markers[0].Crit != 2 {
		t.Errorf("markers = %+v, want only the viewer's crit=2; an outsider's clean marker must not count", p.Markers)
	}
	onlyFork := `{"data":{"repository":{"pullRequests":{"nodes":[{"number":9,"state":"OPEN","headRepository":{"nameWithOwner":"fork/r"}}]}}}}`
	if _, err := parsePR("o/r", "octo", onlyFork); !errors.Is(err, workerwatch.ErrNoPR) {
		t.Errorf("only a fork's PR: err = %v, want ErrNoPR", err)
	}
}

func TestMarkersKeepsEveryViewerMarker(t *testing.T) {
	answer := `{"data":{"repository":{"pullRequests":{"nodes":[{"number":5,"state":"OPEN","headRepository":{"nameWithOwner":"o/r"},"headRefOid":"` + sha + `",
		"reviews":{"nodes":[
			{"body":"<!-- cadence-review: pr-reviewer head=` + sha + ` crit=1 imp=0 -->","author":{"login":"octo"}},
			{"body":"<!-- cadence-review: pr-reviewer head=` + sha + ` crit=0 imp=0 -->","author":{"login":"octo"}},
			{"body":"<!-- cadence-review: pr-reviewer head=ffffffffffffffffffffffffffffffffffffffff crit=0 imp=0 -->","author":{"login":"octo"}}]}}]}}}}`
	p, err := parsePR("o/r", "octo", answer)
	if err != nil {
		t.Fatal(err)
	}
	// No deduplication here: summing is sweep's rule, applied by the
	// collector, and a re-run must not drop the earlier crit=1.
	if len(p.Markers) != 3 || p.Markers[2].Crit != 1 || !p.Markers[2].AtHead || p.Markers[0].AtHead {
		t.Errorf("markers = %+v, want all three, newest first", p.Markers)
	}
	if r := workerwatch.SummarizeMarkers(p.Markers, nil); r == nil || r.Crit != 1 || r.AtHead != 2 || r.Stale != 1 {
		t.Errorf("summary = %+v, want crit=1 over two markers at head, one stale", r)
	}
}

func TestPRForReadsViewerOnceAndRefusesBadInput(t *testing.T) {
	var calls [][]string
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		calls = append(calls, args)
		if strings.Contains(strings.Join(args, " "), "viewer { login }") {
			return `{"data":{"viewer":{"login":"octo"}}}`, nil
		}
		return `{"data":{"repository":{"pullRequests":{"nodes":[{"number":7,"state":"OPEN","headRepository":{"nameWithOwner":"o/r"},"headRefOid":"` + sha + `"}]}}}}`, nil
	}}
	g := &GitHub{Run: fake}
	for range 2 {
		p, err := g.PRFor(context.Background(), "o/r", "feat/x")
		if err != nil || p.Number != 7 {
			t.Fatalf("PRFor = %+v, %v", p, err)
		}
	}
	if len(calls) != 3 {
		t.Errorf("gh calls = %d, want 3 (viewer once, then one per PR read)", len(calls))
	}
	if got := strings.Join(calls[1], " "); !strings.Contains(got, "author=octo") || !strings.Contains(got, "head=feat/x") {
		t.Errorf("PR query args = %s", got)
	}
	for _, bad := range [][2]string{{"o", "b"}, {"o/r", "-b"}, {"o/r", "has space"}, {"-o/r", "b"}} {
		if _, err := g.PRFor(context.Background(), bad[0], bad[1]); err == nil {
			t.Errorf("PRFor(%q, %q) ran gh", bad[0], bad[1])
		}
	}
	if len(calls) != 3 {
		t.Errorf("bad input reached gh: %d calls", len(calls))
	}
}

func TestGitCLIRepo(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "/wt/gl") && strings.Contains(joined, "get-url"):
			return "https://gitlab.com/o/r.git\n", nil
		case strings.Contains(joined, "get-url"):
			return "git@github.com:o/r.git\n", nil
		case strings.Contains(joined, "--show-current"):
			return "feat/x\n", nil
		}
		return "", errors.New("unexpected")
	}}
	g := GitCLI{Run: fake}
	if r, err := g.Repo(context.Background(), "/wt/a"); err != nil || r != "o/r" {
		t.Errorf("Repo = %q, %v", r, err)
	}
	if _, err := g.Repo(context.Background(), "/wt/gl"); !errors.Is(err, workerwatch.ErrNotGitHub) {
		t.Errorf("gitlab origin err = %v, want ErrNotGitHub", err)
	}
	if b, err := g.Branch(context.Background(), "/wt/a"); err != nil || b != "feat/x" {
		t.Errorf("Branch = %q, %v", b, err)
	}
}

func TestGitCLIBranchDirect(t *testing.T) {
	noRun := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		t.Errorf("unexpected git subprocess call: %s %v", name, args)
		return "", errors.New("should not run")
	}}
	g := GitCLI{Run: noRun}

	t.Run("normal .git dir", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Mkdir(filepath.Join(wt, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git", "HEAD"), []byte("ref: refs/heads/feat/direct-head\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := g.Branch(context.Background(), wt)
		if err != nil || got != "feat/direct-head" {
			t.Fatalf("Branch = %q, %v; want %q, nil", got, err, "feat/direct-head")
		}
	})

	t.Run("linked worktree relative gitdir", func(t *testing.T) {
		base := t.TempDir()
		gitDir := filepath.Join(base, "repo", ".git", "worktrees", "wt1")
		wt := filepath.Join(base, "wt1")
		if err := os.MkdirAll(gitDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(wt, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/perf/rel-wt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../repo/.git/worktrees/wt1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := g.Branch(context.Background(), wt)
		if err != nil || got != "perf/rel-wt" {
			t.Fatalf("Branch = %q, %v; want %q, nil", got, err, "perf/rel-wt")
		}
	})

	t.Run("linked worktree absolute gitdir", func(t *testing.T) {
		base := t.TempDir()
		gitDir := filepath.Join(base, "common", "worktrees", "wt2")
		wt := filepath.Join(base, "wt2")
		if err := os.MkdirAll(gitDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(wt, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := g.Branch(context.Background(), wt)
		if err != nil || got != "main" {
			t.Fatalf("Branch = %q, %v; want %q, nil", got, err, "main")
		}
	})

	t.Run("detached HEAD SHA-1 and SHA-256", func(t *testing.T) {
		for _, commit := range []string{
			sha,
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		} {
			wt := t.TempDir()
			if err := os.Mkdir(filepath.Join(wt, ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, ".git", "HEAD"), []byte(commit+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := g.Branch(context.Background(), wt)
			if err != nil || got != "" {
				t.Fatalf("detached(%d) Branch = %q, %v; want \"\", nil", len(commit), got, err)
			}
		}
	})

	t.Run("fallback on unexpected HEAD format", func(t *testing.T) {
		wt := t.TempDir()
		if err := os.Mkdir(filepath.Join(wt, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git", "HEAD"), []byte("ref: refs/tags/v1.0.0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		called := 0
		fallback := GitCLI{Run: &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
			called++
			return "fallback-branch\n", nil
		}}}
		got, err := fallback.Branch(context.Background(), wt)
		if err != nil || got != "fallback-branch" || called != 1 {
			t.Fatalf("fallback Branch = %q, %v (calls=%d); want fallback-branch, nil (calls=1)", got, err, called)
		}
	})
}
