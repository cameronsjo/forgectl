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
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// githubRemoteURL is `git remote get-url origin` output for a github.com
// checkout.
const githubRemoteURL = "git@github.com:cameronsjo/forgectl.git"

// isGetURL matches `git remote get-url <name>`.
func isGetURL(name string, args []string) bool {
	return name == "git" && len(args) >= 2 && args[0] == "remote" && args[1] == "get-url"
}

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
			case isGetURL(name, args):
				return githubRemoteURL, nil
			case name == "gh" && len(args) > 0 && args[0] == "api":
				// A real 404 from gh surfaces as a non-nil error whose message
				// carries the HTTP status — that's the "confirmed gone" signal.
				return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "gh: Not Found (HTTP 404)", Err: errors.New("exit status 1")}
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
			case isGetURL(name, args):
				return githubRemoteURL, nil
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

// TestPrune_RemoteDelete_VerifiesOnTheOriginHost is the #413 regression: `gh
// api` does not infer a host from the checkout, so the verification call must
// name the origin's host. Without --hostname, a GitHub Enterprise checkout's
// verification asked github.com, whose 404 for a repository that does not
// exist there read as "confirmed gone" — a delete reported as verified that
// was never checked. The fake answers 404 ONLY on github.com, so an unpinned
// call is exactly the false success this test must refuse.
func TestPrune_RemoteDelete_VerifiesOnTheOriginHost(t *testing.T) {
	t.Setenv("GH_HOST", "")
	for _, tc := range []struct {
		name     string
		view     string
		wantHost string
	}{
		{"github.com origin", githubRemoteURL, "github.com"},
		{"enterprise https origin", "https://GHE.Example.test/platform/tools.git", "ghe.example.test"},
		{"enterprise ssh origin with port", "ssh://git@ghe.example.test:2222/platform/tools.git", "ghe.example.test"},
		{"credential in https origin", "https://x:SECRET@ghe.example.test/platform/tools.git", "ghe.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{
				RunFunc: func(name string, args []string) (string, error) {
					switch {
					case name == "git" && len(args) > 0 && args[0] == "push":
						return "", nil
					case isGetURL(name, args):
						return tc.view, nil
					case name == "gh" && len(args) > 0 && args[0] == "api":
						if contains(args, "--hostname="+tc.wantHost) {
							// The ref is gone on the host that owns the repo.
							return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "gh: Not Found (HTTP 404)", Err: errors.New("exit status 1")}
						}
						if tc.wantHost == "github.com" {
							return "", errors.New("verification did not name its host")
						}
						// Any other host: github.com answering for a repo it
						// does not have. A 404 here is the false confirmation.
						return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "gh: Not Found (HTTP 404)", Err: errors.New("exit status 1")}
					}
					return "", nil
				},
			}
			item := Classification{
				Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
				Group: SafeToDelete,
			}
			results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "upstream", Remote: true})
			if len(results) != 1 || results[0].Err != nil || !results[0].Deleted {
				t.Fatalf("expected a verified delete, got %+v", results)
			}
			var apiArgs []string
			for _, call := range fake.Calls {
				if call.Name == "gh" && len(call.Args) > 0 && call.Args[0] == "api" {
					apiArgs = call.Args
				}
			}
			if !contains(apiArgs, "--hostname="+tc.wantHost) {
				t.Fatalf("gh api argv = %q, want --hostname=%s (the origin's host, lowercased)", apiArgs, tc.wantHost)
			}
			if strings.Contains(strings.Join(apiArgs, " "), "SECRET") {
				t.Fatalf("gh api argv %q carries the remote URL's credential", apiArgs)
			}
			var gotRemote string
			for _, call := range fake.Calls {
				if isGetURL(call.Name, call.Args) {
					gotRemote = call.Args[len(call.Args)-1]
				}
			}
			if gotRemote != "upstream" {
				t.Fatalf("remote URL read from %q, want the remote the delete went to (upstream)", gotRemote)
			}
		})
	}
}

// TestPrune_RemoteDelete_UnverifiableOriginIsAFailure: when gh's view of the
// origin cannot name a usable host, verification must fail rather than fall
// back to an unpinned query — and the error must not echo the URL.
func TestPrune_RemoteDelete_UnverifiableOriginIsAFailure(t *testing.T) {
	for _, tc := range []struct{ name, view string }{
		{"not a url", "cameronsjo/forgectl"},
		{"port in https url", "https://ghe.example.test:8443/o/r"},
		{"http scheme", "http://ghe.example.test/o/r"},
		{"option-like host", "https://-ghe.example.test/o/r"},
		{"leading-dot host", "https://.hidden/o/r"},
		{"hostile host", "https://gh\x1b[2Je.example.test/o/r"},
		{"credential in unusable url", "https://x:SECRET@gh e.test/o/r"},
		{"hostile owner", "https://github.com/-o/r"},
		{"credential in ported url", "https://x:SECRET@ghe.example.test:8443/o/r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{
				RunFunc: func(name string, args []string) (string, error) {
					if isGetURL(name, args) {
						return tc.view, nil
					}
					if name == "gh" && len(args) > 0 && args[0] == "api" {
						return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "gh: Not Found (HTTP 404)", Err: errors.New("exit status 1")}
					}
					return "", nil
				},
			}
			item := Classification{
				Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
				Group: SafeToDelete,
			}
			results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
			if len(results) != 1 || results[0].Err == nil || results[0].Deleted {
				t.Fatalf("expected an unverified delete to be a failure, got %+v", results)
			}
			for _, call := range fake.Calls {
				if call.Name == "gh" && len(call.Args) > 0 && call.Args[0] == "api" {
					t.Fatalf("gh api ran (%q) though the origin host was unusable", call.Args)
				}
			}
			msg := results[0].Err.Error()
			for _, leak := range []string{"SECRET", "\x1b", `\x1b`, "cameronsjo/forgectl"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("error %q echoes gh output (%q)", msg, leak)
				}
			}
		})
	}
}

// TestPrune_RemoteDelete_StillExists_DoesNotEchoResponse: the "still exists"
// failure names the branch and the endpoint, never gh's response body, which
// is text the server chose (#562).
func TestPrune_RemoteDelete_StillExists_DoesNotEchoResponse(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch {
			case isGetURL(name, args):
				return githubRemoteURL, nil
			case name == "gh" && len(args) > 0 && args[0] == "api":
				return "{\"ref\":\"MARKER\x1b[2J\"}", nil
			}
			return "", nil
		},
	}
	item := Classification{
		Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
		Group: SafeToDelete,
	}
	results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected a reported failure, got %+v", results)
	}
	if msg := results[0].Err.Error(); strings.Contains(msg, "MARKER") || strings.Contains(msg, "\x1b") {
		t.Fatalf("error %q echoes the gh response body", msg)
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

// subprocessFailure is a failed call as the real runner reports it: its text
// is the subprocess's stderr, which for `git push` relays the remote's
// sideband and for gh is host-chosen text (#658).
func subprocessFailure(name string, args []string) error {
	return &exec.CommandError{Name: name, Args: args, Stderr: "remote: MARKER\x1b[2J", ExitCode: 1, Err: errors.New("exit status 1")}
}

func assertNoSubprocessEcho(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	if msg := err.Error(); strings.Contains(msg, "MARKER") || strings.Contains(msg, "\x1b") {
		t.Fatalf("error %q echoes the subprocess's stderr", msg)
	}
	var cmdErr *exec.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error %v lost the CommandError from its chain", err)
	}
}

func TestPrune_RemoteDeleteFailure_DoesNotEchoGitStderr(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "push" {
			return "", subprocessFailure(name, args)
		}
		return "", nil
	}}
	item := Classification{
		Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
		Group: SafeToDelete,
	}
	results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one", results)
	}
	assertNoSubprocessEcho(t, results[0].Err)
}

func TestPrHeadsByState_GhFailure_DoesNotEchoStderr(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", subprocessFailure(name, args)
	}}
	_, err := New(fake).prHeadsByState(context.Background(), "open")
	assertNoSubprocessEcho(t, err)
}

// TestPrune_RemoteDeleteVerifyFailure_DoesNotEchoGhStderr: a non-404 failure
// of the verification GET is reported categorically; its text is gh's
// stderr, which the host chooses (#658).
func TestPrune_RemoteDeleteVerifyFailure_DoesNotEchoGhStderr(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case isGetURL(name, args):
			return githubRemoteURL, nil
		case name == "gh" && len(args) > 0 && args[0] == "api":
			return "", subprocessFailure(name, args)
		}
		return "", nil
	}}
	item := Classification{
		Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
		Group: SafeToDelete,
	}
	results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
	if len(results) != 1 || results[0].Deleted {
		t.Fatalf("results = %+v, want one unverified failure", results)
	}
	assertNoSubprocessEcho(t, results[0].Err)
}

// TestPrune_VerifyFailureOnABranchNamed404IsNotADelete is #749 item 5. The
// verification GET's argv carries the branch name, and CommandError.Error()
// renders that argv, so matching "404" in the error text read a branch named
// fix-404 as verified-deleted when gh failed for another reason (a 502 here).
// Only gh's own "HTTP 404" in stderr confirms the ref is gone.
//
// Mutation: restore strings.Contains(err.Error(), "404") in
// verifyRemoteDeleted and the 502 reads as a successful delete; match a bare
// "HTTP 404" in stderr and the 502 that mentions one does; match
// "(HTTP 404)" anywhere and the "(HTTP 404) (HTTP 502)" row does; drop the
// "gh: HTTP 404" arm and gh's no-message 404 reads as a failure.
func TestPrune_VerifyFailureOnABranchNamed404IsNotADelete(t *testing.T) {
	for _, tc := range []struct {
		stderr      string
		wantDeleted bool
	}{
		{"gh: Server Error (HTTP 502)", false},
		{"gh: Bad Gateway (HTTP 502): upstream said HTTP 404", false},
		{"gh: upstream (HTTP 404) (HTTP 502)", false},
		{"gh: HTTP 404", true},
		{"{\"message\":\"x\"}\ngh: Not Found (HTTP 404)\n", true},
		{"gh: Not Found (HTTP 404)", true},
	} {
		fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
			switch {
			case isGetURL(name, args):
				return githubRemoteURL, nil
			case name == "gh" && len(args) > 0 && args[0] == "api":
				return "", &exec.CommandError{Name: name, Args: args, Stderr: tc.stderr, ExitCode: 1, Err: errors.New("exit status 1")}
			}
			return "", nil
		}}
		item := Classification{
			Info:  Info{Name: "fix-404", RemoteExists: true, MergedOnServer: true},
			Group: SafeToDelete,
		}
		results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
		if len(results) != 1 || results[0].Deleted != tc.wantDeleted || (results[0].Err == nil) != tc.wantDeleted {
			t.Errorf("stderr %q: results = %+v, want deleted=%v", tc.stderr, results, tc.wantDeleted)
		}
	}
}

// TestPrune_ErrIsTerminalSafeByConstruction is #717: every PruneResult.Err is
// safe to print as-is, whatever sink reads it. The branch name and worktree
// path are hostile (a remote refname can carry ESC, C1 controls and U+202E),
// and git's own stderr is never echoed. printPruneResults escapes again, but
// the package must not rely on that one sink.
func TestPrune_ErrIsTerminalSafeByConstruction(t *testing.T) {
	const hostile = "feat/\x1b[2J\u202egpj.exe\u0085x"
	const wtPath = "/tmp/wt\x1b]0;pwned\x07"
	isAPI := func(name string, args []string) bool { return name == "gh" && len(args) > 0 && args[0] == "api" }
	for _, tc := range []struct {
		name string
		opts PruneOptions
		info Info
		run  func(name string, args []string) (string, error)
	}{
		{"worktree remove fails", PruneOptions{Local: true}, Info{Name: hostile, LocalExists: true, WorktreePath: wtPath},
			func(name string, args []string) (string, error) {
				if name == "git" && args[0] == "worktree" {
					return "", subprocessFailure(name, args)
				}
				return "", nil
			}},
		{"branch -D fails", PruneOptions{Local: true}, Info{Name: hostile, LocalExists: true},
			func(name string, args []string) (string, error) {
				if name == "git" && args[0] == "branch" {
					return "", subprocessFailure(name, args)
				}
				return "", nil
			}},
		{"push --delete fails", PruneOptions{RemoteName: "up\x1b[31m", Remote: true}, Info{Name: hostile, RemoteExists: true},
			func(name string, args []string) (string, error) {
				if name == "git" && args[0] == "push" {
					return "", subprocessFailure(name, args)
				}
				return "", nil
			}},
		{"remote unresolvable", PruneOptions{RemoteName: "origin", Remote: true}, Info{Name: hostile, RemoteExists: true},
			func(name string, args []string) (string, error) {
				if isGetURL(name, args) {
					return "", subprocessFailure(name, args)
				}
				return "", nil
			}},
		{"still exists", PruneOptions{RemoteName: "origin", Remote: true}, Info{Name: hostile, RemoteExists: true},
			func(name string, args []string) (string, error) {
				if isGetURL(name, args) {
					return githubRemoteURL, nil
				}
				return "", nil
			}},
		{"verify fails", PruneOptions{RemoteName: "origin", Remote: true}, Info{Name: hostile, RemoteExists: true},
			func(name string, args []string) (string, error) {
				switch {
				case isGetURL(name, args):
					return githubRemoteURL, nil
				case isAPI(name, args):
					return "", subprocessFailure(name, args)
				}
				return "", nil
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.info.MergedOnServer = true
			fake := &exec.FakeRunner{RunFunc: tc.run}
			item := Classification{Info: tc.info, Group: SafeToDelete}
			results := New(fake).Prune(context.Background(), []Classification{item}, tc.opts)
			if len(results) != 1 || results[0].Err == nil || results[0].Deleted {
				t.Fatalf("results = %+v, want one failure", results)
			}
			msg := results[0].Err.Error()
			for _, r := range msg {
				if termsafe.IsUnsafeTerminalRune(r) {
					t.Fatalf("Err %q carries raw unsafe rune %U", msg, r)
				}
			}
			if strings.Contains(msg, "MARKER") {
				t.Fatalf("Err %q echoes git/gh stderr", msg)
			}
			if !strings.Contains(msg, `\x1b[2J`) {
				t.Fatalf("Err %q does not name the branch in escaped form", msg)
			}
		})
	}
}

// TestPrune_RemoteDelete_VerifiesAgainstThePushURL is #707: `git push
// --delete` goes to the remote's push URL, so verification must ask about
// that repository. A triangular remote fetches from upstream and pushes to a
// fork; checking the fetch URL asked upstream, whose 404 for a branch it
// never had read as a confirmed delete while the branch survived on the fork.
func TestPrune_RemoteDelete_VerifiesAgainstThePushURL(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch {
			case name == "git" && len(args) > 0 && args[0] == "push":
				return "", nil
			case isGetURL(name, args) && contains(args, "--push"):
				return "git@github.com:fork/tools.git", nil
			case isGetURL(name, args):
				return "https://github.com/upstream/tools.git", nil
			case name == "gh" && len(args) > 0 && args[0] == "api":
				if contains(args, "repos/fork/tools/git/ref/heads/feat/done") {
					// The delete did not take on the fork: the ref is still there.
					return `{"ref":"refs/heads/feat/done"}`, nil
				}
				return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "gh: Not Found (HTTP 404)", Err: errors.New("exit status 1")}
			}
			return "", nil
		},
	}
	item := Classification{
		Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
		Group: SafeToDelete,
	}
	results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
	if len(results) != 1 || results[0].Err == nil || results[0].Deleted {
		t.Fatalf("expected the surviving fork branch to fail verification, got %+v", results)
	}
	if !strings.Contains(results[0].Err.Error(), "still exists") {
		t.Fatalf("Err = %v, want the still-exists verdict from the fork", results[0].Err)
	}
}

// TestPrune_RemoteDelete_SeveralPushURLsCannotVerify: `git push --delete`
// pushes to every push URL, and get-url without --all prints only the first.
// Verification reads all of them (--all) and refuses more than one rather
// than checking one repository and leaving the rest unverified.
func TestPrune_RemoteDelete_SeveralPushURLsCannotVerify(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch {
			case isGetURL(name, args) && contains(args, "--all"):
				return "git@github.com:a/tools.git\ngit@github.com:b/tools.git\n", nil
			case isGetURL(name, args):
				// get-url --push without --all: the first URL only, exit 0.
				return "git@github.com:a/tools.git", nil
			case name == "gh" && len(args) > 0 && args[0] == "api":
				return "", &exec.CommandError{Name: "gh", Args: args, Stderr: "gh: Not Found (HTTP 404)", Err: errors.New("exit status 1")}
			}
			return "", nil
		},
	}
	item := Classification{
		Info:  Info{Name: "feat/done", RemoteExists: true, MergedOnServer: true},
		Group: SafeToDelete,
	}
	results := New(fake).Prune(context.Background(), []Classification{item}, PruneOptions{RemoteName: "origin", Remote: true})
	if len(results) != 1 || results[0].Err == nil || results[0].Deleted {
		t.Fatalf("expected an unverifiable delete to be a failure, got %+v", results)
	}
	if !strings.Contains(results[0].Err.Error(), "more than one push URL") {
		t.Fatalf("Err = %v, want the several-push-URLs refusal", results[0].Err)
	}
	for _, call := range fake.Calls {
		if call.Name == "gh" {
			t.Fatalf("gh ran (%q) though the remote has two push URLs", call.Args)
		}
	}
}
