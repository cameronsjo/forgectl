package branch

// Test plan for branch.go / classify.go
//
// Classify (pure)
//   [x] Protected branch is always Blocked
//   [x] Open PR always Blocked, even merged-on-server + worktree-attached
//       (gotcha #2 outranks gotcha #1/#3)
//   [x] MergedOnServer + no worktree -> SafeToDelete
//   [x] MergedOnServer + worktree -> SafeToDelete, WorktreePath preserved
//   [x] MergedLocally=true, MergedOnServer=false -> NEVER SafeToDelete
//   [x] UpstreamGone, no MergedOnServer -> NeedsAttention, not SafeToDelete
//   [x] No signal at all -> Blocked ("appears active"), never dropped
//
// Prune / gotcha assertions against exec.FakeRunner.Calls
//   [x] (a) remote-delete verification calls the SINGULAR
//       repos/{o}/{r}/git/ref/heads/{branch} endpoint, never the PLURAL
//       .../git/refs/heads/{branch} form
//   [x] (b) `git worktree remove` is issued, and completes, BEFORE
//       `git branch -D` for a worktree-attached branch
//   [x] (c) a branch with an open PR never appears in ANY delete argv — Prune
//       issues zero Runner calls for it
//   [x] (d) full Enumerate round-trip: a squash-merged branch (absent from
//       `git branch --merged`, present in `gh pr list --state merged`) is
//       classified SafeToDelete — gotcha #1 actually handled, not just
//       documented
//   [x] Prune uses `-D` (force), not `-d`, for the local delete
//   [x] Enumerate skips a gone-but-unconfirmed branch unless IncludeGone

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// --- Classify (pure) --------------------------------------------------

func TestClassify_ProtectedBranch_AlwaysBlocked(t *testing.T) {
	c := Classify(Info{Name: "main", Protected: true, MergedOnServer: true})
	if c.Group != Blocked {
		t.Fatalf("Group = %v, want Blocked (reason: %s)", c.Group, c.Reason)
	}
}

func TestClassify_OpenPR_AlwaysBlocked_EvenMergedAndWorktreed(t *testing.T) {
	c := Classify(Info{
		Name:           "feat/stacked",
		OpenPRNumber:   9,
		MergedOnServer: true,
		WorktreePath:   "/tmp/wt",
	})
	if c.Group != Blocked {
		t.Fatalf("Group = %v, want Blocked (an open PR must outrank a server-confirmed merge); reason: %s", c.Group, c.Reason)
	}
}

func TestClassify_MergedOnServer_NoWorktree_SafeToDelete(t *testing.T) {
	c := Classify(Info{Name: "feat/done", MergedOnServer: true})
	if c.Group != SafeToDelete {
		t.Fatalf("Group = %v, want SafeToDelete (reason: %s)", c.Group, c.Reason)
	}
}

func TestClassify_MergedOnServer_Worktree_StillSafeToDelete(t *testing.T) {
	c := Classify(Info{Name: "feat/done", MergedOnServer: true, WorktreePath: "/tmp/wt"})
	if c.Group != SafeToDelete {
		t.Fatalf("Group = %v, want SafeToDelete (Prune, not Classify, handles worktree order); reason: %s", c.Group, c.Reason)
	}
	if c.Info.WorktreePath != "/tmp/wt" {
		t.Errorf("WorktreePath not preserved on the classification: %+v", c.Info)
	}
}

// TestClassify_LocalMergedOnly_NeverSafeToDelete is the direct gotcha #1/#5
// assertion at the classify layer: a branch git's own `--merged` thinks is
// merged, but gh has no record of a merged PR for, must never be
// SafeToDelete — Classify does not trust local-only merge detection.
func TestClassify_LocalMergedOnly_NeverSafeToDelete(t *testing.T) {
	c := Classify(Info{Name: "feat/local-only", MergedLocally: true, MergedOnServer: false})
	if c.Group == SafeToDelete {
		t.Fatalf("Group = SafeToDelete based on MergedLocally alone — gotcha #1/#5 violated (reason: %s)", c.Reason)
	}
}

func TestClassify_UpstreamGone_NoServerConfirmation_NeedsAttention(t *testing.T) {
	c := Classify(Info{Name: "feat/stale", UpstreamGone: true})
	if c.Group != NeedsAttention {
		t.Fatalf("Group = %v, want NeedsAttention (a gone upstream is not proof of merge); reason: %s", c.Group, c.Reason)
	}
}

func TestClassify_NoSignal_BlockedAsActive(t *testing.T) {
	c := Classify(Info{Name: "feat/in-progress"})
	if c.Group != Blocked {
		t.Fatalf("Group = %v, want Blocked (\"appears active\"); reason: %s", c.Group, c.Reason)
	}
}

// --- Prune gotcha (a): singular verification endpoint ------------------

func TestPrune_RemoteDelete_VerifiesViaSingularEndpoint_NeverPlural(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch {
			case name == "git" && len(args) > 0 && args[0] == "push":
				return "", nil
			case name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "view":
				return "cameronsjo/forgectl", nil
			case name == "gh" && len(args) > 0 && args[0] == "api":
				// A real 404 from gh surfaces as a non-nil error whose message
				// carries the HTTP status — that's the "confirmed gone" signal.
				return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "HTTP 404: Not Found (https://api.github.com/...)", Err: errors.New("exit status 1")}
			}
			return "", nil
		},
	}
	client := New(fake)

	item := Classification{
		Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
		Group: SafeToDelete,
	}
	results := client.Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
	if len(results) != 1 || results[0].Err != nil || !results[0].Deleted {
		t.Fatalf("expected a successful delete, got %+v", results)
	}

	var apiCall *exec.Call
	for i := range fake.Calls {
		if fake.Calls[i].Name == "gh" && len(fake.Calls[i].Args) > 0 && fake.Calls[i].Args[0] == "api" {
			apiCall = &fake.Calls[i]
		}
	}
	if apiCall == nil {
		t.Fatal("expected a `gh api` verification call, found none")
	}
	path := apiCall.Args[len(apiCall.Args)-1]
	if !strings.Contains(path, "git/ref/heads/") {
		t.Errorf("verification path %q does not use the SINGULAR git/ref/heads endpoint", path)
	}
	if strings.Contains(path, "git/refs/heads/") {
		t.Errorf("verification path %q uses the PLURAL git/refs/heads endpoint — that endpoint returns 200/[] for a "+
			"gone branch and would mask a failed delete as a success (gotcha #4)", path)
	}
}

// TestPrune_RemoteDelete_StillExists_IsAFailure locks the flip side of gotcha
// #4: if the singular endpoint answers successfully (branch still exists),
// Prune must report a failure, not silently treat the push as done.
func TestPrune_RemoteDelete_StillExists_IsAFailure(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch {
			case name == "git" && len(args) > 0 && args[0] == "push":
				return "", nil
			case name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "view":
				return "cameronsjo/forgectl", nil
			case name == "gh" && len(args) > 0 && args[0] == "api":
				// 200 with a real ref object — the branch is still there.
				return `{"ref":"refs/heads/feat/done"}`, nil
			}
			return "", nil
		},
	}
	client := New(fake)

	item := Classification{
		Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
		Group: SafeToDelete,
	}
	results := client.Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
	if len(results) != 1 || results[0].Err == nil || results[0].Deleted {
		t.Fatalf("expected a reported failure when the branch still exists post-delete, got %+v", results)
	}
}

// --- Prune gotcha (b): worktree removed before branch delete -----------

func TestPrune_WorktreeRemovedBeforeLocalBranchDelete(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "", nil },
	}
	client := New(fake)

	item := Classification{
		Info:  Info{Name: "feat/wt", LocalExists: true, MergedOnServer: true, WorktreePath: "/tmp/wt"},
		Group: SafeToDelete,
	}
	results := client.Prune(context.Background(), []Classification{item}, PruneOptions{Local: true})
	if len(results) != 1 || results[0].Err != nil || !results[0].Deleted {
		t.Fatalf("expected a successful delete, got %+v", results)
	}

	if len(fake.Calls) != 2 {
		t.Fatalf("expected exactly 2 Runner calls (worktree remove, branch -D), got %d: %+v", len(fake.Calls), fake.Calls)
	}
	first, second := fake.Calls[0], fake.Calls[1]
	if first.Name != "git" || first.Args[0] != "worktree" || first.Args[1] != "remove" {
		t.Errorf("call[0] = %+v, want `git worktree remove`", first)
	}
	if !contains(first.Args, "/tmp/wt") {
		t.Errorf("call[0] args %v do not include the worktree path", first.Args)
	}
	if second.Name != "git" || second.Args[0] != "branch" {
		t.Errorf("call[1] = %+v, want `git branch -D` — worktree remove MUST precede branch delete", second)
	}
	if !contains(second.Args, "-D") {
		t.Errorf("call[1] args %v do not use -D (force) — see gotcha #5 doc on why -d would fail here", second.Args)
	}
	if contains(second.Args, "-d") {
		t.Errorf("call[1] args %v use -d, which would refuse a squash-merged branch exactly like --merged does", second.Args)
	}
}

// TestPrune_WorktreeRemoveFails_BranchDeleteNeverAttempted proves the order
// is enforced, not just usually-correct: if the worktree remove errors,
// `git branch -D` must never run at all.
func TestPrune_WorktreeRemoveFails_BranchDeleteNeverAttempted(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" && len(args) > 0 && args[0] == "worktree" {
				return "", errors.New("fatal: '/tmp/wt' contains modified or untracked files, use --force")
			}
			return "", nil
		},
	}
	client := New(fake)

	item := Classification{
		Info:  Info{Name: "feat/wt", LocalExists: true, MergedOnServer: true, WorktreePath: "/tmp/wt"},
		Group: SafeToDelete,
	}
	results := client.Prune(context.Background(), []Classification{item}, PruneOptions{Local: true})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected a reported failure, got %+v", results)
	}
	for _, c := range fake.Calls {
		if c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "branch" {
			t.Errorf("git branch must never run after a failed worktree remove, got call %+v", c)
		}
	}
}

// --- Prune gotcha (c): an open-PR branch never reaches delete argv ------

func TestPrune_OpenPRBranch_NeverDeleted_ZeroRunnerCalls(t *testing.T) {
	fake := &exec.FakeRunner{}
	client := New(fake)

	blocked := Classify(Info{Name: "feat/in-flight", LocalExists: true, RemoteExists: true, OpenPRNumber: 42})
	if blocked.Group != Blocked {
		t.Fatalf("precondition failed: expected Blocked, got %v", blocked.Group)
	}

	results := client.Prune(context.Background(), []Classification{blocked}, PruneOptions{Local: true, Remote: true})
	if len(results) != 1 || !results[0].Skipped || results[0].Deleted || results[0].Err != nil {
		t.Fatalf("expected a clean skip, got %+v", results)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("a branch with an open PR must never appear in ANY delete argv — expected zero Runner calls, got %+v", fake.Calls)
	}
}

// --- Prune gotcha (d): squash-merged branch is safe-to-delete -----------

// TestEnumerate_SquashMergedBranch_SafeToDelete is the full round-trip proof
// of gotcha #1: `git branch --merged` never lists the squash-merged branch
// (it is not a literal ancestor of main), yet `gh pr list --state merged`
// does report it — Enumerate must classify it SafeToDelete anyway.
func TestEnumerate_SquashMergedBranch_SafeToDelete(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch {
			case name == "git" && len(args) > 0 && args[0] == "for-each-ref" && contains(args, "refs/heads"):
				return "main\t\t\nfeat/squashed\t\t\n", nil
			case name == "git" && len(args) > 0 && args[0] == "for-each-ref" && contains(args, "refs/remotes/origin"):
				// Includes the origin/HEAD symref row, exactly as real git emits
				// it (see TestRemoteBranches_ExcludesHeadSymref) — its
				// %(refname:short) is the bare "origin", not "origin/HEAD".
				return "refs/remotes/origin/HEAD\torigin\n" +
					"refs/remotes/origin/main\torigin/main\n" +
					"refs/remotes/origin/feat/squashed\torigin/feat/squashed\n", nil
			case name == "git" && len(args) > 0 && args[0] == "worktree":
				return "worktree /repo\nHEAD deadbeef\nbranch refs/heads/main\n", nil
			case name == "git" && len(args) > 0 && args[0] == "branch" && contains(args, "--merged"):
				// Deliberately does NOT include feat/squashed: a squash merge is
				// never a literal ancestor of main, so local --merged is blind to
				// it. This is the exact case gotcha #1 exists to cover.
				return "main\n", nil
			case name == "gh" && len(args) > 1 && args[0] == "pr" && args[1] == "list" && contains(args, "open"):
				return "[]", nil
			case name == "gh" && len(args) > 1 && args[0] == "pr" && args[1] == "list" && contains(args, "merged"):
				return `[{"number":7,"headRefName":"feat/squashed"}]`, nil
			}
			return "", nil
		},
	}
	client := New(fake, WithRemoteName("origin"), WithDefaultBranch("main"))

	report, err := client.Enumerate(context.Background(), EnumerateOptions{Local: true, Remote: true})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}

	var found *Classification
	for i := range report.SafeToDelete {
		if report.SafeToDelete[i].Info.Name == "feat/squashed" {
			found = &report.SafeToDelete[i]
		}
	}
	if found == nil {
		t.Fatalf("expected feat/squashed in SafeToDelete, got report: %+v", report)
	}
	if found.Info.MergedLocally {
		t.Errorf("expected MergedLocally=false (a squash merge is never a literal ancestor) — got true, the test fixture is wrong")
	}
	if !found.Info.MergedOnServer {
		t.Errorf("expected MergedOnServer=true (gh pr list --state merged reported it)")
	}

	// main itself must never show up as a delete candidate.
	for _, c := range report.SafeToDelete {
		if c.Info.Name == "main" {
			t.Errorf("the default branch must never classify SafeToDelete, got %+v", c)
		}
	}

	// The origin/HEAD symref (see TestRemoteBranches_ExcludesHeadSymref) must
	// never surface as a spurious "origin" branch anywhere in the report.
	for _, group := range [][]Classification{report.SafeToDelete, report.Blocked, report.NeedsAttention} {
		for _, c := range group {
			if c.Info.Name == "origin" {
				t.Errorf("origin/HEAD symref leaked into the report as a bare %q branch: %+v", c.Info.Name, c)
			}
		}
	}
}

// TestRemoteBranches_ExcludesHeadSymref is a direct regression test for a bug
// caught via a manual dry-run against this repo's own origin remote:
// refs/remotes/<remote>/HEAD is a symref, and git's %(refname:short) renders
// it as the BARE remote name ("origin"), not "origin/HEAD" as the full
// refname would suggest. A naive `name == "HEAD"` check (after stripping the
// "<remote>/" prefix) never catches this, because "origin" doesn't carry that
// prefix to strip in the first place — remoteBranches must filter by the
// FULL refname instead.
func TestRemoteBranches_ExcludesHeadSymref(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" && len(args) > 0 && args[0] == "for-each-ref" {
				return "refs/remotes/origin/HEAD\torigin\n" +
					"refs/remotes/origin/main\torigin/main\n" +
					"refs/remotes/origin/feat/x\torigin/feat/x\n", nil
			}
			return "", nil
		},
	}
	client := New(fake, WithRemoteName("origin"))

	names, err := client.remoteBranches(context.Background(), "origin")
	if err != nil {
		t.Fatalf("remoteBranches: %v", err)
	}
	for _, n := range names {
		if n == "origin" || n == "HEAD" {
			t.Errorf("origin/HEAD symref must be excluded, got branch name %q in %v", n, names)
		}
	}
	if !contains(names, "main") || !contains(names, "feat/x") {
		t.Errorf("expected real branches main and feat/x, got %v", names)
	}
}

// TestEnumerate_GoneBranch_OmittedByDefault_SurfacedWithIncludeGone covers
// the --include-gone flag's effect on a plain gone-but-unconfirmed branch.
func TestEnumerate_GoneBranch_OmittedByDefault_SurfacedWithIncludeGone(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch {
			case name == "git" && len(args) > 0 && args[0] == "for-each-ref" && contains(args, "refs/heads"):
				return "main\t\t\nfeat/deleted-upstream\torigin/feat/deleted-upstream\t[gone]\n", nil
			case name == "git" && len(args) > 0 && args[0] == "worktree":
				return "", nil
			case name == "git" && len(args) > 0 && args[0] == "branch" && contains(args, "--merged"):
				return "main\n", nil
			case name == "gh":
				return "[]", nil
			}
			return "", nil
		},
	}
	client := New(fake, WithDefaultBranch("main"))

	report, err := client.Enumerate(context.Background(), EnumerateOptions{Local: true})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	for _, group := range [][]Classification{report.SafeToDelete, report.Blocked, report.NeedsAttention} {
		for _, c := range group {
			if c.Info.Name == "feat/deleted-upstream" {
				t.Fatalf("expected feat/deleted-upstream omitted by default, found in report: %+v", c)
			}
		}
	}

	report, err = client.Enumerate(context.Background(), EnumerateOptions{Local: true, IncludeGone: true})
	if err != nil {
		t.Fatalf("Enumerate with IncludeGone: %v", err)
	}
	var found bool
	for _, c := range report.NeedsAttention {
		if c.Info.Name == "feat/deleted-upstream" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected feat/deleted-upstream in NeedsAttention with --include-gone, got report: %+v", report)
	}
}
