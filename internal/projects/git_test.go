package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// v2Hash is a stand-in object name for porcelain-v2 fixtures. The parser must
// never interpret an object name, so one repeated value across every fixture
// keeps the records realistic without implying the contents matter.
const v2Hash = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"

// Complete, realistic porcelain-v2 records — one per record class git can
// emit. Fixtures compose these rather than hand-writing partial lines, so a
// test that means "one modified file" cannot accidentally also be testing
// truncation.
const (
	v2Ordinary  = "1 .M N... 100644 100644 100644 " + v2Hash + " " + v2Hash + " file.go"
	v2Unmerged  = "u UU N... 100644 100644 100644 100644 " + v2Hash + " " + v2Hash + " " + v2Hash + " conflict.go"
	v2Untracked = "? newfile.go"
	v2Ignored   = "! build/artifact.o"
)

// v2Rename is a rename record: the TAB between target and source is the
// physical separator git documents, so it is built rather than written inline
// where an editor could turn it into spaces.
var v2Rename = "2 R. N... 100644 100644 100644 " + v2Hash + " " + v2Hash + " R100 new.go" + "\t" + "old.go"

// v2Branch renders the branch header block git emits for a tracking branch
// that is ahead/behind by the given counts.
func v2Branch(ahead, behind int) string {
	return "# branch.oid " + v2Hash + "\n" +
		"# branch.head main\n" +
		"# branch.upstream origin/main\n" +
		"# branch.ab +" + strconv.Itoa(ahead) + " -" + strconv.Itoa(behind)
}

// v2Out joins a branch header block and zero or more records into one
// porcelain-v2 payload.
func v2Out(parts ...string) string { return strings.Join(parts, "\n") }

// ctxRunner is a minimal context-aware stand-in for the Runner seam.
// exec.FakeRunner deliberately drops the context (its Run signature ignores
// it), so cancellation behavior cannot be expressed through it; rather than
// weaken that contract for every test in the repo, the two tests that need to
// observe a canceled probe use this local double.
type ctxRunner struct {
	fn func(ctx context.Context, name string, args []string) (string, error)

	mu    sync.Mutex
	calls []exec.Call
}

func (r *ctxRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, exec.Call{Name: name, Args: args})
	r.mu.Unlock()
	return r.fn(ctx, name, args)
}

func (r *ctxRunner) Calls() []exec.Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]exec.Call(nil), r.calls...)
}

// countGitSubcommands tallies recorded git calls by their subcommand — the
// token after `-C <dir>`. Tests assert on these counts rather than on a call's
// index, because Inventory's fan-out makes completion order undefined.
func countGitSubcommands(calls []exec.Call, dir string) map[string]int {
	counts := map[string]int{}
	for _, c := range calls {
		if c.Name != "git" || len(c.Args) < 3 || c.Args[0] != "-C" {
			continue
		}
		if dir != "" && c.Args[1] != dir {
			continue
		}
		counts[c.Args[2]]++
	}
	return counts
}

// TestGitStatus_NonRepoDir_StateIsNotRepo pins the first of the three
// collapsed states: a directory with no .git must report StatusNotRepo, not
// the zero-value-shaped "clean". A sibling dir that DOES have .git, wired to
// the same FakeRunner, is the control — the only variable across the two
// subtests is .git's presence, so a pass here isolates the .git check itself.
func TestGitStatus_NonRepoDir_StateIsNotRepo(t *testing.T) {
	tmp := t.TempDir()
	nonRepo := filepath.Join(tmp, "notarepo")
	if err := os.MkdirAll(nonRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "realrepo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", nil // clean status, 0 ahead
	}}

	if got := gitStatus(context.Background(), fake, nonRepo); got.State != StatusNotRepo {
		t.Errorf("non-repo dir: State = %q, want %q", got.State, StatusNotRepo)
	}
	// Control: same runner, a dir that IS a repo must report StatusOK.
	if got := gitStatus(context.Background(), fake, repo); got.State != StatusOK {
		t.Errorf("control repo dir: State = %q, want %q", got.State, StatusOK)
	}
}

// TestGitStatus_CommandError_StateIsUnknown pins the third collapsed state —
// the one no existing test covered — a `git status` failure (corrupt repo,
// permissions, git missing) must report StatusUnknown, explicitly not
// StatusOK, so a failed check is never mistaken for a clean tree.
func TestGitStatus_CommandError_StateIsUnknown(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "brokenrepo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	failing := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", errors.New("fatal: unable to read current working directory")
	}}
	if got := gitStatus(context.Background(), failing, repo); got.State != StatusUnknown {
		t.Errorf("failing git status: State = %q, want %q", got.State, StatusUnknown)
	}

	// Control: identical fixture, RunFunc succeeds → StatusOK.
	succeeding := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", nil
	}}
	if got := gitStatus(context.Background(), succeeding, repo); got.State != StatusOK {
		t.Errorf("control succeeding git status: State = %q, want %q", got.State, StatusOK)
	}
}

// TestGitStatus_UsesOnePorcelainV2BranchProbe is the core of forgectl#216: a
// supported repository is probed with exactly one git process. The old shape
// ran `status --porcelain` and then, on a clean tree, a second `rev-list
// --count @{upstream}..HEAD` to learn the ahead count; porcelain v2's
// `--branch` header carries that count in the same output, so the second
// process is pure overhead. The assertions pin all three properties that make
// the collapse real — the exact argv, the call count, and the ahead value
// still arriving — because any two of them can hold while the third regresses.
func TestGitStatus_UsesOnePorcelainV2BranchProbe(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "clean")
	repo := filepath.Join(tmp, "clean")

	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return v2Branch(2, 3), nil
	}}

	got := gitStatus(context.Background(), fake, repo)
	want := GitStatus{State: StatusOK, Ahead: 2}
	if got != want {
		t.Errorf("gitStatus = %+v, want %+v", got, want)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("git calls = %d (%v), want exactly 1", len(fake.Calls), fake.Calls)
	}
	wantArgs := []string{"-C", repo, "status", "--porcelain=v2", "--branch"}
	if fake.Calls[0].Name != "git" || !reflect.DeepEqual(fake.Calls[0].Args, wantArgs) {
		t.Errorf("argv = %s %v, want git %v", fake.Calls[0].Name, fake.Calls[0].Args, wantArgs)
	}
	counts := countGitSubcommands(fake.Calls, repo)
	if counts["rev-list"] != 0 {
		t.Errorf("rev-list calls = %d, want 0 (the ahead count comes from branch.ab now)", counts["rev-list"])
	}
}

// TestGitStatus_CombinedCommandFailureIsUnknownWithoutFallback pins the one
// deliberate behavior change of forgectl#216. Folding the branch-graph
// calculation into the status command means a graph failure — or a
// cancellation — now makes the single command exit nonzero, where the old
// shape could return StatusOK with a silently-swallowed rev-list error. The
// safe answer is StatusUnknown, which PullAll already refuses to mutate.
//
// The assertions are about process SHAPE, not just the state: a v1 retry or a
// version probe after the failure would still produce StatusUnknown while
// blowing the process budget and risking reclassifying an unreadable tree.
func TestGitStatus_CombinedCommandFailureIsUnknownWithoutFallback(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "repo")
	repo := filepath.Join(tmp, "repo")

	cases := []struct {
		name string
		fn   func(ctx context.Context, name string, args []string) (string, error)
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{
			name: "ordinary command error",
			fn: func(context.Context, string, []string) (string, error) {
				return "", errors.New("fatal: bad object HEAD")
			},
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
		},
		{
			name: "context cancellation",
			fn: func(ctx context.Context, _ string, _ []string) (string, error) {
				return "", ctx.Err()
			},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			run := &ctxRunner{fn: tc.fn}

			if got := gitStatus(ctx, run, repo); got.State != StatusUnknown {
				t.Errorf("gitStatus = %+v, want State %q", got, StatusUnknown)
			}

			calls := run.Calls()
			if len(calls) != 1 {
				t.Fatalf("git calls = %d (%v), want exactly 1 attempted probe", len(calls), calls)
			}
			wantArgs := []string{"-C", repo, "status", "--porcelain=v2", "--branch"}
			if !reflect.DeepEqual(calls[0].Args, wantArgs) {
				t.Errorf("argv = %v, want %v", calls[0].Args, wantArgs)
			}
			counts := countGitSubcommands(calls, repo)
			if counts["rev-list"] != 0 {
				t.Errorf("rev-list calls = %d, want 0 (no fallback after a failed probe)", counts["rev-list"])
			}
			if counts["version"] != 0 || counts["--version"] != 0 {
				t.Errorf("version probe ran (%v); an ambiguous command error is not a version signal", counts)
			}
		})
	}
}

// TestGitStatus_ParsesPorcelainV2 is the protocol matrix. It runs through
// gitStatus rather than calling the parser directly, so the fixtures prove
// what a caller actually observes and no test-only production API is needed.
//
// The three groups are deliberately different contracts. Valid records map to
// counts. Malformed or unknown RECORDS fail closed to StatusUnknown, because
// silently ignoring a dirty record git meant to report would let PullAll
// rebase over uncommitted work. Malformed branch.ab HEADERS fail soft to
// ahead zero, because git documents headers as an extensible set and the
// ahead count is cosmetic — losing it costs a label, not safety.
func TestGitStatus_ParsesPorcelainV2(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want GitStatus
	}{
		// --- valid mappings ---
		{"clean ahead 2 behind 3", v2Branch(2, 3), GitStatus{State: StatusOK, Ahead: 2}},
		{"clean behind only", v2Branch(0, 7), GitStatus{State: StatusOK}},
		{
			"no upstream",
			"# branch.oid " + v2Hash + "\n# branch.head main",
			GitStatus{State: StatusOK},
		},
		{
			"detached head",
			"# branch.oid " + v2Hash + "\n# branch.head (detached)",
			GitStatus{State: StatusOK},
		},
		{
			"unborn branch",
			"# branch.oid (initial)\n# branch.head main",
			GitStatus{State: StatusOK},
		},
		{"empty output", "", GitStatus{State: StatusOK}},
		{"ordinary record", v2Out(v2Branch(1, 0), v2Ordinary), GitStatus{State: StatusOK, Modified: 1}},
		{"rename record", v2Out(v2Branch(1, 0), v2Rename), GitStatus{State: StatusOK, Modified: 1}},
		{"unmerged record", v2Out(v2Branch(1, 0), v2Unmerged), GitStatus{State: StatusOK, Modified: 1}},
		{"untracked record", v2Out(v2Branch(1, 0), v2Untracked), GitStatus{State: StatusOK, Untracked: 1}},
		{"ignored record counts nothing", v2Out(v2Branch(1, 0), v2Ignored), GitStatus{State: StatusOK, Ahead: 1}},
		{
			"mixed tracked untracked ignored discards ahead",
			v2Out(v2Branch(4, 1), v2Ordinary, v2Rename, v2Unmerged, v2Untracked, v2Ignored),
			GitStatus{State: StatusOK, Modified: 3, Untracked: 1},
		},
		{
			"unknown header is harmless",
			v2Out("# future.header whatever it says", v2Branch(2, 0)),
			GitStatus{State: StatusOK, Ahead: 2},
		},
		{
			"headers in an unexpected order",
			v2Out("# branch.ab +5 -0", "# branch.upstream origin/main", "# branch.head main", "# branch.oid "+v2Hash),
			GitStatus{State: StatusOK, Ahead: 5},
		},
		{
			"blank lines are ignored",
			v2Out(v2Branch(3, 0), "", v2Ordinary, ""),
			GitStatus{State: StatusOK, Modified: 1},
		},

		// --- path boundaries: C quoting must not forge a second record ---
		{
			"c-quoted untracked path with escaped newline tab quote backslash",
			v2Out(v2Branch(1, 0), `? "line\n1 forged\tquote\"slash\\"`),
			GitStatus{State: StatusOK, Untracked: 1},
		},
		{
			"c-quoted rename with escaped content around the literal tab separator",
			v2Out(v2Branch(1, 0),
				"2 R. N... 100644 100644 100644 "+v2Hash+" "+v2Hash+" R100 "+
					`"new\tname\n1.go"`+"\t"+`"old\"name\\.go"`),
			GitStatus{State: StatusOK, Modified: 1},
		},

		// --- fail closed on records ---
		{"truncated ordinary record", v2Out(v2Branch(0, 0), "1 .M N... 100644"), GitStatus{State: StatusUnknown}},
		{
			"ordinary record with an empty path",
			v2Out(v2Branch(0, 0), "1 .M N... 100644 100644 100644 "+v2Hash+" "+v2Hash+" "),
			GitStatus{State: StatusUnknown},
		},
		{"truncated rename record", v2Out(v2Branch(0, 0), "2 R. N... 100644 100644"), GitStatus{State: StatusUnknown}},
		{
			"rename record without the literal tab separator",
			v2Out(v2Branch(0, 0), "2 R. N... 100644 100644 100644 "+v2Hash+" "+v2Hash+" R100 new.go old.go"),
			GitStatus{State: StatusUnknown},
		},
		{
			"rename record with an empty source path",
			v2Out(v2Branch(0, 0), "2 R. N... 100644 100644 100644 "+v2Hash+" "+v2Hash+" R100 new.go\t"),
			GitStatus{State: StatusUnknown},
		},
		{"truncated unmerged record", v2Out(v2Branch(0, 0), "u UU N... 100644 100644"), GitStatus{State: StatusUnknown}},
		{"truncated untracked record", v2Out(v2Branch(0, 0), "?"), GitStatus{State: StatusUnknown}},
		{"untracked record with an empty path", v2Out(v2Branch(0, 0), "? "), GitStatus{State: StatusUnknown}},
		{"truncated ignored record", v2Out(v2Branch(0, 0), "!"), GitStatus{State: StatusUnknown}},
		{"unknown record discriminator", v2Out(v2Branch(0, 0), "x something new"), GitStatus{State: StatusUnknown}},
		{"bare hash is not a header", v2Out("#", v2Ordinary), GitStatus{State: StatusUnknown}},
		{
			"v1 output is not silently accepted",
			" M file.go\n?? newfile.go",
			GitStatus{State: StatusUnknown},
		},

		// --- fail soft on branch.ab: valid records, ahead drops to zero ---
		{"branch.ab missing plus sign", v2Out("# branch.ab 2 -3"), GitStatus{State: StatusOK}},
		{"branch.ab missing minus sign", v2Out("# branch.ab +2 3"), GitStatus{State: StatusOK}},
		{"branch.ab missing behind token", v2Out("# branch.ab +2"), GitStatus{State: StatusOK}},
		{"branch.ab missing ahead token", v2Out("# branch.ab -3"), GitStatus{State: StatusOK}},
		{"branch.ab extra token", v2Out("# branch.ab +2 -3 -4"), GitStatus{State: StatusOK}},
		{"branch.ab non-decimal ahead", v2Out("# branch.ab +2a -3"), GitStatus{State: StatusOK}},
		{"branch.ab non-decimal behind", v2Out("# branch.ab +2 -3b"), GitStatus{State: StatusOK}},
		{"branch.ab encoded negative ahead", v2Out("# branch.ab +-1 -3"), GitStatus{State: StatusOK}},
		{"branch.ab encoded negative behind", v2Out("# branch.ab +1 --1"), GitStatus{State: StatusOK}},
		{"branch.ab ahead overflow", v2Out("# branch.ab +99999999999999999999 -0"), GitStatus{State: StatusOK}},
		{"branch.ab behind overflow", v2Out("# branch.ab +1 -99999999999999999999"), GitStatus{State: StatusOK}},
		{"branch.ab empty magnitudes", v2Out("# branch.ab + -"), GitStatus{State: StatusOK}},
		{
			"one malformed duplicate branch.ab forces ahead zero",
			v2Out("# branch.ab +4 -0", "# branch.ab +bad -0"),
			GitStatus{State: StatusOK},
		},
		{
			"repeated valid branch.ab uses the last value",
			v2Out("# branch.ab +4 -0", "# branch.ab +9 -0"),
			GitStatus{State: StatusOK, Ahead: 9},
		},
		{
			"ahead is discarded on a dirty tree",
			v2Out(v2Branch(6, 0), v2Untracked),
			GitStatus{State: StatusOK, Untracked: 1},
		},
	}

	tmp := t.TempDir()
	mkGitDir(t, tmp, "repo")
	repo := filepath.Join(tmp, "repo")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
				return tc.out, nil
			}}
			if got := gitStatus(context.Background(), fake, repo); got != tc.want {
				t.Errorf("gitStatus = %+v, want %+v\n--- output ---\n%s", got, tc.want, tc.out)
			}
			if len(fake.Calls) != 1 {
				t.Errorf("git calls = %d, want exactly 1", len(fake.Calls))
			}
		})
	}
}

// TestGitStatusV2_RecordOrderDoesNotAffectCounts pins that the parser holds no
// order-dependent state: git documents record order as undefined, so a
// permutation that changed the counts would be a real bug against real git
// output, not a theoretical one.
func TestGitStatusV2_RecordOrderDoesNotAffectCounts(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "repo")
	repo := filepath.Join(tmp, "repo")

	records := []string{v2Ordinary, v2Rename, v2Unmerged, v2Untracked, v2Ignored}
	want := GitStatus{State: StatusOK, Modified: 3, Untracked: 1}

	// Rotations plus a reversal: enough permutations to falsify any
	// position-sensitive parse without enumerating all 120.
	var perms [][]string
	for i := range records {
		rot := append(append([]string(nil), records[i:]...), records[:i]...)
		perms = append(perms, rot)
	}
	reversed := make([]string, len(records))
	for i, r := range records {
		reversed[len(records)-1-i] = r
	}
	perms = append(perms, reversed)

	for _, perm := range perms {
		out := v2Out(append([]string{v2Branch(2, 0)}, perm...)...)
		fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, nil }}
		if got := gitStatus(context.Background(), fake, repo); got != want {
			t.Errorf("permutation %v: gitStatus = %+v, want %+v", perm, got, want)
		}
	}
}
