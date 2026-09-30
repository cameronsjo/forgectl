package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// TestGitProjectBranch pins the one git call the header makes and how its
// two lines are read.
func TestGitProjectBranch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		out           string
		err           error
		project, head string
		ok            bool
	}{
		{"branch", "/home/u/Projects/forgectl\nmain\n", nil, "forgectl", "main", true},
		{"detached", "/w/repo\nHEAD\n", nil, "repo", "(detached)", true},
		{"not a repo", "", errors.New("exit 128"), "", "", false},
		{"one line", "/w/repo\n", nil, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				if name != "git" || strings.Join(args, " ") != "rev-parse --show-toplevel --abbrev-ref HEAD" {
					t.Errorf("unexpected call %s %v", name, args)
				}
				return tc.out, tc.err
			}}
			project, branch, ok := gitProjectBranch(context.Background(), fake)
			if project != tc.project || branch != tc.head || ok != tc.ok {
				t.Errorf("gitProjectBranch = (%q, %q, %v), want (%q, %q, %v)", project, branch, ok, tc.project, tc.head, tc.ok)
			}
		})
	}
}

// TestReviewCounts_AbsentStoreIsUnavailableAndUncreated pins that opening the
// hub never creates the pr sessions directory, and that no store reads as
// unavailable rather than as zero reviews.
func TestReviewCounts_AbsentStoreIsUnavailableAndUncreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pr-sessions")
	client := pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(dir))
	if _, _, ok := reviewCounts(context.Background(), client); ok {
		t.Error("reviewCounts reported a count for a store that does not exist")
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Error("reviewCounts created the sessions directory")
	}
}

// TestReviewCounts_EmptyStoreCountsZero is the control for the test above:
// an existing, empty store is available and counts nothing.
func TestReviewCounts_EmptyStoreCountsZero(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pr-sessions")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	client := pr.New(&exec.FakeRunner{}, pr.WithSessionsDir(dir))
	running, queued, ok := reviewCounts(context.Background(), client)
	if !ok || running != 0 || queued != 0 {
		t.Errorf("reviewCounts = (%d, %d, %v), want (0, 0, true)", running, queued, ok)
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
