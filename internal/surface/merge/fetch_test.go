package merge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

const rowBase1204 = "2e469107d375d1977085887813de99ce18bc91bf"

// fixtureGH answers the reads for #1204 from the captured fixtures.
type fixtureGH struct {
	t        *testing.T
	override map[string]string // a key's substring to a replacement body
	failOn   string
	// notFoundOn makes the matching call fail as gh does on HTTP 404.
	notFoundOn string
}

func (g fixtureGH) runner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "gh" {
			return "", errors.New("not gh")
		}
		joined := strings.Join(args, " ")
		if g.failOn != "" && strings.Contains(joined, g.failOn) {
			return "", errors.New("HTTP 502")
		}
		if g.notFoundOn != "" && strings.Contains(joined, g.notFoundOn) {
			return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "gh: Not Found (HTTP 404)", ExitCode: 1, Err: errors.New("exit status 1")}
		}
		for k, v := range g.override {
			if strings.Contains(joined, k) {
				return v, nil
			}
		}
		file := ""
		switch {
		case strings.Contains(joined, "viewer { login databaseId }"):
			file = "discover_1204.json"
		case strings.Contains(joined, "reviewThreads(first: 100)"):
			file = "pr_1204.json"
		case strings.Contains(joined, "checkSuites(first: 50)"):
			file = "checks_1204.json"
		case strings.Contains(joined, "compare/"+base1204+"..."+head1204):
			file = "compare_1204.json"
		case strings.Contains(joined, "compare/"+rowBase1204+"..."):
			file = "compare_ahead.json"
		case strings.Contains(joined, "git/trees/"):
			ref := joined[strings.Index(joined, "git/trees/")+len("git/trees/"):]
			sha, dir, _ := strings.Cut(ref, ":")
			file = "tree_" + sha[:8] + "_" + strings.ReplaceAll(dir, "/", "_") + ".json"
		default:
			g.t.Fatalf("unexpected gh call: %s", joined)
		}
		return string(readFixture(g.t, file)), nil
	}}
}

type memCache map[string][]byte

func (m memCache) Read(head string) ([]byte, error) { return m[head], nil }
func (m memCache) Write(head string, data []byte) error {
	m[head] = data
	return nil
}

func row1204() Row {
	return Row{
		Name: "gh1175-forgectl", Branch: "worker/gh1175-forgectl", BranchFrom: "new", LaunchID: "launch-abc", Stage: "launched",
		Base: rowBase1204, GitHubRepo: "cameronsjo/forgectl", GitHubRepoID: forgectlID, QueueLaunchID: "launch-abc", QueueState: "reported",
	}
}

func TestReaderRead1204(t *testing.T) {
	run := fixtureGH{t: t}.runner()
	cache := memCache{}
	snap, err := Reader{GH: run, Cache: cache}.Read(context.Background(), row1204())
	if err != nil {
		t.Fatal(err)
	}
	f := snap.Facts
	if !snap.HasPR || f.PR.Number != 1204 || f.PR.HeadRefOid != head1204 || f.OperatorID != operatorID || f.Repository.DefaultBranch != "main" {
		t.Fatalf("snapshot %+v", snap)
	}
	if len(f.Files) != 4 || f.Files[0].BaseMode != "100644" || f.Files[0].HeadMode != "100644" {
		t.Fatalf("files %+v", f.Files)
	}
	if f.BaseAncestry != CompareAhead || f.HeadAncestry != CompareAhead {
		t.Fatalf("ancestry %q %q", f.BaseAncestry, f.HeadAncestry)
	}
	if len(snap.Statuses) != 1 || !strings.Contains(snap.Statuses[0], "CodeRabbit") {
		t.Fatalf("statuses %q", snap.Statuses)
	}
	for _, c := range run.Calls {
		if len(c.Args) < 3 || c.Args[0] != "api" || !strings.Contains(strings.Join(c.Args, " "), "--hostname github.com") {
			t.Fatalf("a gh call without --hostname github.com: %q", c.Args)
		}
	}
	if _, ok := cache[head1204]; !ok {
		t.Fatal("the file list was not cached")
	}
	// The real #1204 is merged and touches internal/selfupdate: the verdict
	// refuses on both.
	v := evalManual(f)
	wantRefusal(t, v, "the PR is MERGED")
	wantRefusal(t, v, "internal/selfupdate/selfupdate.go")

	again := fixtureGH{t: t}.runner()
	snap2, err := Reader{GH: again, Cache: cache}.Read(context.Background(), row1204())
	if err != nil || !snap2.Cached || len(snap2.Facts.Files) != 4 {
		t.Fatalf("cached read: %+v, %v", snap2, err)
	}
	for _, c := range again.Calls {
		if j := strings.Join(c.Args, " "); strings.Contains(j, "git/trees/") || strings.Contains(j, "compare/"+base1204) {
			t.Fatalf("a cached read still read %s", j)
		}
	}
	// The merge path passes no cache and reads everything.
	nocache := fixtureGH{t: t}.runner()
	if snap3, err := (Reader{GH: nocache}).Read(context.Background(), row1204()); err != nil || snap3.Cached {
		t.Fatalf("uncached read: %+v, %v", snap3, err)
	}
}

func TestReaderReadRefuses(t *testing.T) {
	ctx := context.Background()
	if _, err := (Reader{GH: fixtureGH{t: t}.runner()}).Read(ctx, Row{Name: "x", Branch: "worker/x"}); !errors.Is(err, ErrNoRecordedRepo) {
		t.Fatalf("no recorded repo: %v", err)
	}
	for _, fail := range []string{"viewer { login", "reviewThreads(first", "checkSuites(first", "compare/" + base1204, "git/trees/", "compare/" + rowBase1204} {
		_, err := Reader{GH: fixtureGH{t: t, failOn: fail}.runner()}.Read(ctx, row1204())
		if !errors.Is(err, ErrRead) {
			t.Errorf("failing %q: %v, want ErrRead", fail, err)
		}
	}
	moved := strings.Replace(string(readFixture(t, "checks_1204.json")), `"headRefOid": "`+head1204, `"headRefOid": "`+base1204, 1)
	if _, err := (Reader{GH: fixtureGH{t: t, override: map[string]string{"checkSuites(first": moved}}.runner()}).Read(ctx, row1204()); !errors.Is(err, ErrRead) || !strings.Contains(err.Error(), "moved during the read") {
		t.Fatalf("head moved: %v", err)
	}
	noPR := strings.Replace(string(readFixture(t, "discover_1204.json")), `"headRefName": "worker/gh1175-forgectl"`, `"headRefName": "worker/other"`, 1)
	snap, err := Reader{GH: fixtureGH{t: t, override: map[string]string{"viewer { login": noPR}}.runner()}.Read(ctx, row1204())
	if err != nil || snap.HasPR || !strings.Contains(snap.NoPR, "no pull request") {
		t.Fatalf("no PR: %+v, %v", snap, err)
	}
}

// TestReaderReadBaseNotOnGitHub: a recorded base GitHub does not have makes
// the ancestry compares answer 404, which is a refusal reason, not a failed
// read.
func TestReaderReadBaseNotOnGitHub(t *testing.T) {
	snap, err := Reader{GH: fixtureGH{t: t, notFoundOn: "compare/" + rowBase1204}.runner()}.Read(context.Background(), row1204())
	if err != nil {
		t.Fatalf("a 404 compare failed the read: %v", err)
	}
	if snap.Facts.BaseAncestry != CompareNotFound || snap.Facts.HeadAncestry != CompareNotFound {
		t.Fatalf("ancestry %q %q, want %q", snap.Facts.BaseAncestry, snap.Facts.HeadAncestry, CompareNotFound)
	}
	v := evalManual(snap.Facts)
	wantRefusal(t, v, "the worker's recorded base 2e469107d375 is not on GitHub")
	if slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.Contains(r, "compare status") }) {
		t.Fatalf("reasons %q; the 404 should be said once, not as an ancestry status", v.Reasons)
	}
	// Any other failure of the same compare is still a failed read.
	if _, err := (Reader{GH: fixtureGH{t: t, failOn: "compare/" + rowBase1204}.runner()}).Read(context.Background(), row1204()); !errors.Is(err, ErrRead) {
		t.Fatalf("a 502 compare: %v, want ErrRead", err)
	}
}
