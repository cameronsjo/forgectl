package cli

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

const (
	testBaseSHA = "0123456789abcdef0123456789abcdef01234567"
	testHeadSHA = "89abcdef0123456789abcdef0123456789abcdef"
)

// baseFake answers workerBase's git and gh calls: origin, the GitHub default
// branch and its head, the fetch, the fetched ref, and the checkout's HEAD.
func baseFake(origin, branch, apiSHA, fetchedSHA string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "gh" && strings.Contains(joined, "/commits/"):
			return apiSHA + "\n", nil
		case name == "gh":
			return branch + "\n", nil
		case strings.Contains(joined, "get-url origin"):
			if origin == "" {
				return "", errors.New("no such remote")
			}
			return origin + "\n", nil
		case strings.Contains(joined, "rev-parse") && strings.Contains(joined, "HEAD^{commit}"):
			return testHeadSHA + "\n", nil
		case strings.Contains(joined, "rev-parse"):
			if fetchedSHA == "" {
				return "", errors.New("unknown revision")
			}
			return fetchedSHA + "\n", nil
		}
		return "", nil
	}}
}

// fetchArgs returns the git fetch call's arguments, or nil.
func fetchArgs(run *exec.FakeRunner) []string {
	for _, c := range run.Calls {
		if i := slices.Index(c.Args, "fetch"); i >= 0 {
			return c.Args[i:]
		}
	}
	return nil
}

func TestWorkerBase(t *testing.T) {
	run := baseFake("git@github.com:o/r.git", "main", testBaseSHA, testBaseSHA)
	got, err := workerBase(t.Context(), run, "/top", io.Discard)
	if err != nil || got != testBaseSHA {
		t.Fatalf("workerBase = %q, %v, want %s", got, err, testBaseSHA)
	}
	f := fetchArgs(run)
	if len(f) != 4 || !slices.Equal(f[:3], []string{"fetch", "--no-tags", "origin"}) || !strings.HasPrefix(f[3], "+refs/heads/main:refs/forgectl/base/"+testBaseSHA+"-") {
		t.Fatalf("fetch %q, want +refs/heads/main into a per-launch refs/forgectl/base/<sha>-<nonce>", f)
	}
	if got, err := workerBase(t.Context(), baseFake("ssh://git@ssh.github.com:443/o/r.git", "main", testBaseSHA, testBaseSHA), "/top", io.Discard); err != nil || got != testBaseSHA {
		t.Fatalf("an ssh.github.com origin gave %q, %v", got, err)
	}

	for name, run := range map[string]*exec.FakeRunner{
		"origin is not on github.com": baseFake("git@gitlab.com:o/r.git", "main", testBaseSHA, testBaseSHA),
		"there is no origin":          baseFake("", "main", testBaseSHA, testBaseSHA),
	} {
		t.Run("falls back to HEAD: "+name, func(t *testing.T) {
			if got, err := workerBase(t.Context(), run, "/top", io.Discard); err != nil || got != testHeadSHA {
				t.Fatalf("workerBase = %q, %v, want the checkout's HEAD", got, err)
			}
			if fetchArgs(run) != nil {
				t.Fatal("fetched for a non-GitHub origin")
			}
		})
	}

	for name, run := range map[string]*exec.FakeRunner{
		"GitHub names no commit":            baseFake("git@github.com:o/r.git", "main", "not-a-sha", testBaseSHA),
		"the branch moved during the fetch": baseFake("git@github.com:o/r.git", "main", testBaseSHA, strings.Repeat("f", 40)),
		"the fetch landed nothing":          baseFake("git@github.com:o/r.git", "main", testBaseSHA, ""),
		"GitHub names a dash branch":        baseFake("git@github.com:o/r.git", "-x", testBaseSHA, testBaseSHA),
		"GitHub names a branch with #":      baseFake("git@github.com:o/r.git", "a#b", testBaseSHA, testBaseSHA),
		"gh fails":                          ghFailing(),
	} {
		t.Run("refuses: "+name, func(t *testing.T) {
			if got, err := workerBase(t.Context(), run, "/top", io.Discard); err == nil {
				t.Fatalf("workerBase accepted it: %q", got)
			}
		})
	}
}

func TestEscapeRefPath(t *testing.T) {
	if got := escapeRefPath("release/1.0 x"); got != "release/1.0%20x" {
		t.Fatalf("escapeRefPath = %q", got)
	}
}

// ghFailing is a GitHub origin whose gh calls fail (offline, logged out):
// the launch must fail, not quietly fall back to the checkout's HEAD.
func ghFailing() *exec.FakeRunner {
	run := baseFake("git@github.com:o/r.git", "main", testBaseSHA, testBaseSHA)
	inner := run.RunFunc
	run.RunFunc = func(name string, args []string) (string, error) {
		if name == "gh" {
			return "", errors.New("gh: not logged in")
		}
		return inner(name, args)
	}
	return run
}

// TestWorkerBase_FallbackSaysWhy pins forgectl#1129: an SSH host alias falls
// back to HEAD with one line naming why, and a trailing-slash GitHub URL no
// longer falls back at all.
func TestWorkerBase_FallbackSaysWhy(t *testing.T) {
	var note bytes.Buffer
	got, err := workerBase(t.Context(), baseFake("git@gh-work:o/r.git", "main", testBaseSHA, testBaseSHA), "/top", &note)
	if err != nil || got != testHeadSHA {
		t.Fatalf("an ssh alias gave %q, %v, want the checkout's HEAD", got, err)
	}
	if out := note.String(); strings.Count(out, "\n") != 1 || !strings.Contains(out, `"gh-work"`) || strings.Contains(out, "o/r") {
		t.Fatalf("fallback note = %q, want one line naming the host and not the URL", out)
	}

	note.Reset()
	got, err = workerBase(t.Context(), baseFake("https://github.com/o/r/", "main", testBaseSHA, testBaseSHA), "/top", &note)
	if err != nil || got != testBaseSHA || note.Len() != 0 {
		t.Fatalf("a trailing-slash origin gave %q, %v, note %q, want GitHub's head and no note", got, err, note.String())
	}
}
