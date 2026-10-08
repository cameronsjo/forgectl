package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func fixedSources() hubHeaderSources {
	return hubHeaderSources{
		git:     func(context.Context) (string, string, bool) { return "forgectl", "main", true },
		tmux:    func(context.Context) (int, bool) { return 3, true },
		reviews: func(context.Context) (int, int, bool) { return 1, 2, true },
	}
}

// TestGatherHubHeader_AllSourcesAvailable is the control: every field lands.
func TestGatherHubHeader_AllSourcesAvailable(t *testing.T) {
	h := gatherHubHeader(context.Background(), fixedSources(), time.Second)
	if got, want := h.Line(), "forgectl @ main · 3 tmux · 1 review running, 2 queued"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
}

// TestGatherHubHeader_OmitsUnavailableFields pins forgectl#730 item 1: a
// source that fails drops its field, and the others still render.
func TestGatherHubHeader_OmitsUnavailableFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*hubHeaderSources)
		want   string
		absent string
	}{
		{"git", func(s *hubHeaderSources) {
			s.git = func(context.Context) (string, string, bool) { return "leak", "leak", false }
		}, "3 tmux · 1 review running, 2 queued", "leak"},
		{"tmux", func(s *hubHeaderSources) {
			s.tmux = func(context.Context) (int, bool) { return 99, false }
		}, "forgectl @ main · 1 review running, 2 queued", "99"},
		{"reviews", func(s *hubHeaderSources) {
			s.reviews = func(context.Context) (int, int, bool) { return 7, 7, false }
		}, "forgectl @ main · 3 tmux", "7"},
		{"none", func(s *hubHeaderSources) { *s = hubHeaderSources{} }, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fixedSources()
			tc.edit(&src)
			got := gatherHubHeader(context.Background(), src, time.Second).Line()
			if got != tc.want {
				t.Errorf("header = %q, want %q", got, tc.want)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Errorf("header %q carries the unavailable source's value %q", got, tc.absent)
			}
		})
	}
}

// TestGatherHubHeader_StalledSourceIsOmittedOnTime pins the budget: a source
// that never answers (a wedged tmux server) is dropped when the budget runs
// out, and the hub opens without waiting on it.
func TestGatherHubHeader_StalledSourceIsOmittedOnTime(t *testing.T) {
	src := fixedSources()
	release := make(chan struct{})
	defer close(release)
	src.tmux = func(context.Context) (int, bool) {
		<-release
		return 5, true
	}
	start := time.Now()
	h := gatherHubHeader(context.Background(), src, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("gatherHubHeader waited %v on a stalled source", elapsed)
	}
	if h.HasTmux {
		t.Error("a stalled tmux source still produced a field")
	}
	if h.Project != "forgectl" || !h.HasReviews {
		t.Errorf("the prompt sources were lost with the stalled one: %+v", h)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGitProjectBranch pins the header's .git read: branch, detached, a repo
// with no commits yet (the case `git rev-parse --abbrev-ref HEAD` fails on),
// a linked worktree's .git pointer file, a subdirectory, and no checkout.
func TestGitProjectBranch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "forgectl", ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(root, "unborn", ".git", "HEAD"), "ref: refs/heads/trunk\n") // no objects, no refs
	writeFile(t, filepath.Join(root, "detached", ".git", "HEAD"), strings.Repeat("a1", 20)+"\n")
	writeFile(t, filepath.Join(root, "broken", ".git", "HEAD"), "garbage\n")
	writeFile(t, filepath.Join(root, "gitdirs", "wt", "HEAD"), "ref: refs/heads/feat/x\n")
	writeFile(t, filepath.Join(root, "linked", ".git"), "gitdir: "+filepath.Join(root, "gitdirs", "wt")+"\n")
	if err := os.MkdirAll(filepath.Join(root, "forgectl", "internal", "cli"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "plain"), 0o750); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		dir, project, branch string
		ok                   bool
	}{
		{"forgectl", "forgectl", "main", true},
		{"forgectl/internal/cli", "forgectl", "main", true},
		{"unborn", "unborn", "trunk", true},
		{"detached", "detached", "(detached)", true},
		{"broken", "broken", "", true},
		{"linked", "linked", "feat/x", true},
	} {
		project, branch, ok := gitProjectBranch(filepath.Join(root, tc.dir))
		if project != tc.project || branch != tc.branch || ok != tc.ok {
			t.Errorf("gitProjectBranch(%s) = (%q, %q, %v), want (%q, %q, %v)", tc.dir, project, branch, ok, tc.project, tc.branch, tc.ok)
		}
	}
	// "plain" sits under root, which is inside no checkout of its own; walk
	// only as far as the temp root's real ancestors allow.
	if project, _, ok := gitProjectBranch(filepath.Join(root, "plain")); ok && project == "plain" {
		t.Errorf("a directory with no .git reported itself as a checkout")
	}
}

// storeState records every entry in dir by name, size, mode, and mtime.
func storeState(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, e.Name()+"|"+info.Mode().String()+"|"+strconv.FormatInt(info.Size(), 10)+"|"+info.ModTime().Format(time.RFC3339Nano))
	}
	return strings.Join(rows, "\n")
}

// TestHubHeader_ReviewsReadWritesNothing pins the review's Important 2: a
// header gather over a store holding a record creates, changes, and locks
// nothing — no lifecycle lock file appears.
func TestHubHeader_ReviewsReadWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pr-sessions")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	client := pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(dir))
	if _, err := client.Queue(context.Background(), pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}, pr.PrepareOpts{}); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	// Queue took the lifecycle lock; remove its file so the gather is shown
	// not to create one.
	if err := os.Remove(filepath.Join(dir, ".pr-session-lifecycle.lock")); err != nil {
		t.Fatalf("remove setup lock: %v", err)
	}
	before := storeState(t, dir)

	h := gatherHubHeader(context.Background(), liveHubHeaderSources(nil, client), time.Second)

	if after := storeState(t, dir); after != before {
		t.Errorf("a header gather changed the review store:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the store holds %d entries after the gather, want only the record", len(entries))
	}
	if !h.HasReviews || h.ReviewsQueued != 1 || h.ReviewsRunning != 0 {
		t.Errorf("header = %+v, want one queued review read from the store", h)
	}
}

// TestHubHeader_AbsentStoreIsUnavailableAndUncreated pins that opening the hub
// never creates the pr sessions directory, and that no store reads as
// unavailable rather than as zero reviews.
func TestHubHeader_AbsentStoreIsUnavailableAndUncreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pr-sessions")
	h := gatherHubHeader(context.Background(), liveHubHeaderSources(nil, pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(dir))), time.Second)
	if h.HasReviews {
		t.Error("the header reported review counts for a store that does not exist")
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Error("the header gather created the sessions directory")
	}
}

// TestHubHeader_EmptyStoreCountsZero is the control for the test above.
func TestHubHeader_EmptyStoreCountsZero(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pr-sessions")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := gatherHubHeader(context.Background(), liveHubHeaderSources(nil, pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(dir))), time.Second)
	if !h.HasReviews || h.ReviewsRunning != 0 || h.ReviewsQueued != 0 {
		t.Errorf("header = %+v, want an available zero count", h)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the gather wrote %d entries into an empty store", len(entries))
	}
}

// TestHubRunLine_QuotesThePickerArgument pins the stderr echo before a
// hub-selected command runs: a picker argument is one element, shown as one
// element, and nothing in it reaches the terminal raw.
func TestHubRunLine_QuotesThePickerArgument(t *testing.T) {
	th := theme.Default()
	if got := hubRunLine(th, []string{"projects", "clone", "a b"}); !strings.Contains(got, "$ forgectl projects clone 'a b'") {
		t.Errorf("hubRunLine = %q, want the argument quoted as one element", got)
	}
	termsafetest.AssertInert(t, "hubRunLine", hubRunLine(th, []string{"pr", termsafetest.Hostile("x")}))
}

// With no resolvable home the picker offers nothing rather than listing the
// working directory: the hub then opens with free text (forgectl#730).
func TestHubArgSources_UnresolvedRootOffersNoNames(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "proj", ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	t.Setenv("PROJECTS_DIR", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("this platform resolves a home without HOME")
	}
	names := hubArgSources()["projects pick"](context.Background())
	if len(names) != 0 {
		t.Errorf("picker names = %v, want none when the root cannot be resolved", names)
	}
}
