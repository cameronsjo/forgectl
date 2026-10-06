package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// baseFake answers workerBase's git and gh calls: origin, the GitHub default
// branch and its head, the fetch, and the fetched ref.
func baseFake(origin, branch, apiSHA, fetchedSHA string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "gh" && strings.HasSuffix(args[1], "/commits/"+branch):
			return apiSHA + "\n", nil
		case name == "gh":
			return branch + "\n", nil
		case strings.Contains(joined, "get-url origin"):
			return origin + "\n", nil
		case strings.Contains(joined, "rev-parse"):
			if fetchedSHA == "" {
				return "", errors.New("unknown revision")
			}
			return fetchedSHA + "\n", nil
		}
		return "", nil
	}}
}

func TestWorkerBase(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	got, err := workerBase(t.Context(), baseFake("git@github.com:o/r.git", "main", sha, sha), "/top")
	if err != nil || got != sha {
		t.Fatalf("workerBase = %q, %v, want %s", got, err, sha)
	}

	refused := map[string]*exec.FakeRunner{
		"origin is not on github.com":         baseFake("git@gitlab.com:o/r.git", "main", sha, sha),
		"GitHub names no commit":              baseFake("git@github.com:o/r.git", "main", "not-a-sha", sha),
		"the fetched ref differs from GitHub": baseFake("git@github.com:o/r.git", "main", sha, strings.Repeat("f", 40)),
		"the fetch landed nothing":            baseFake("git@github.com:o/r.git", "main", sha, ""),
		"GitHub names a dash branch":          baseFake("git@github.com:o/r.git", "-x", sha, sha),
	}
	for name, run := range refused {
		t.Run("refuses: "+name, func(t *testing.T) {
			if got, err := workerBase(t.Context(), run, "/top"); err == nil {
				t.Fatalf("workerBase accepted it: %q", got)
			}
		})
	}
}
