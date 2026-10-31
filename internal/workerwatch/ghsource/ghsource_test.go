package ghsource

import (
	"context"
	"errors"
	"fmt"
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
			nodes = append(nodes, fmt.Sprintf(`{"body":%q}`, r))
		}
		return `{"data":{"repository":{"pullRequests":{"nodes":[{"number":12,"state":"OPEN","isDraft":true,"headRefOid":"` + sha +
			`","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING"}}}]},"reviews":{"nodes":[` + strings.Join(nodes, ",") + `]}}]}}}}`
	}
	p, err := parsePR("o/r", body(
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
	if p.Marker == nil || p.Marker.Reviewer != "security-reviewer" || p.Marker.AtHead {
		t.Errorf("marker = %+v, want the newest (security-reviewer, stale)", p.Marker)
	}

	p, _ = parsePR("o/r", body("<!-- cadence-review: pr-reviewer head="+sha+" crit=0 imp=3 -->"))
	if p.Marker == nil || !p.Marker.AtHead || p.Marker.Imp != 3 {
		t.Errorf("marker at head = %+v", p.Marker)
	}

	if _, err := parsePR("o/r", `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`); !errors.Is(err, workerwatch.ErrNoPR) {
		t.Errorf("no nodes: err = %v, want workerwatch.ErrNoPR", err)
	}
	if _, err := parsePR("o/r", `{"errors":[{"message":"x"}]}`); err == nil || errors.Is(err, workerwatch.ErrNoPR) {
		t.Errorf("errors answer: err = %v, want an unusable-answer error", err)
	}
	bad, _ := parsePR("o/r", strings.Replace(strings.Replace(body(), "PENDING", "WEIRD", 1), sha, "not-a-sha", 1))
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

func TestPRForReadsViewerOnceAndRefusesBadInput(t *testing.T) {
	var calls [][]string
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		calls = append(calls, args)
		if strings.Contains(strings.Join(args, " "), "viewer { login }") {
			return `{"data":{"viewer":{"login":"octo"}}}`, nil
		}
		return `{"data":{"repository":{"pullRequests":{"nodes":[{"number":7,"state":"OPEN","headRefOid":"` + sha + `"}]}}}}`, nil
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
