package pr

// Test plan for local.go
//
// PrepareLocal (Classification: ops layer, offline clean-room — no gh, no network)
//   [x] Dry-run: only the two local, read-only `git rev-parse` calls happen;
//       no worktree, no quarantine/allowlist write, no breadcrumb
//   [x] Real: uses `git worktree add`, never `git clone`
//   [x] Real: the worktree ref is the resolved HEAD oid, not the literal "HEAD"
//   [x] localRef output round-trips through ParseRef (the Number<=0 failure mode)
//   [x] The findings dir is a sibling of workspace, never nested inside it
//   [x] The findings dir is created under the client's durable findingsDir
//       (config.PrFindingsDir by default), not a sibling of the OS-temp
//       workspace, and keeps the forgectl-findings- prefix
// localProfile (Classification: deny-by-default security control, broader than PR mode)
//   [x] Denies every gh subcommand; grants none
//   [x] Exactly one scoped Write(findingsDir/**) grant; no bare "Write" in deny

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

const localHeadOid = "deadbeefcafe1234567890abcdef1234567890"

// localGitRunner fakes the two `git -C <path> rev-parse …` calls PrepareLocal
// issues, plus a no-op success for the later `git worktree add`.
func localGitRunner() *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" && len(args) >= 3 && args[2] == "rev-parse" {
				if contains(args, "--abbrev-ref") {
					return "main", nil
				}
				return localHeadOid, nil
			}
			return "", nil // worktree add / tmux succeed as no-ops
		},
	}
}

func TestPrepareLocal_DryRunCreatesNothing(t *testing.T) {
	fake := localGitRunner()
	c := testClient(t, fake)
	path := t.TempDir()

	sess, err := c.PrepareLocal(context.Background(), path, PrepareLocalOpts{DryRun: true})
	if err != nil {
		t.Fatalf("PrepareLocal dry-run: %v", err)
	}
	if !sess.Local {
		t.Error("dry-run session should be marked Local")
	}
	if sess.Workspace != "" || sess.Path != "" || sess.FindingsDir != "" {
		t.Errorf("dry-run created state: workspace=%q path=%q findings=%q", sess.Workspace, sess.Path, sess.FindingsDir)
	}
	if sess.HeadRef != "main" || sess.HeadOid != localHeadOid {
		t.Errorf("head metadata not resolved: %+v", sess)
	}

	if len(fake.Calls) != 2 {
		t.Fatalf("dry-run should issue exactly two Runner calls; got %+v", fake.Calls)
	}
	for _, call := range fake.Calls {
		if call.Name != "git" {
			t.Errorf("dry-run issued a non-git call: %+v", call)
		}
	}
	if _, ok := findCall(fake.Calls, "tmux"); ok {
		t.Error("dry-run must not run tmux")
	}

	// error ignored: SessionsDir may not exist yet on dry-run, which reads the
	// same as "exists but empty" for this assertion
	entries, _ := os.ReadDir(c.SessionsDir())
	if len(entries) != 0 {
		t.Errorf("dry-run wrote breadcrumbs: %v", entries)
	}
}

func TestPrepareLocal_UsesWorktreeNotClone(t *testing.T) {
	fake := localGitRunner()
	c := testClient(t, fake)
	path := t.TempDir()

	sess, err := c.PrepareLocal(context.Background(), path, PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(sess.Workspace)
		os.RemoveAll(sess.FindingsDir)
	})

	var sawWorktree bool
	for _, call := range fake.Calls {
		if call.Name != "git" {
			continue
		}
		if contains(call.Args, "clone") {
			t.Errorf("local review must never git clone; call: %+v", call)
		}
		if contains(call.Args, "worktree") && contains(call.Args, "add") {
			sawWorktree = true
		}
	}
	if !sawWorktree {
		t.Errorf("expected a git worktree add call; got %+v", fake.Calls)
	}
}

func TestPrepareLocal_PinsToResolvedOid(t *testing.T) {
	fake := localGitRunner()
	c := testClient(t, fake)
	path := t.TempDir()

	sess, err := c.PrepareLocal(context.Background(), path, PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(sess.Workspace)
		os.RemoveAll(sess.FindingsDir)
	})

	var worktreeCall exec.Call
	var found bool
	for _, call := range fake.Calls {
		if call.Name == "git" && contains(call.Args, "worktree") {
			worktreeCall = call
			found = true
		}
	}
	if !found {
		t.Fatalf("no git worktree call found; got %+v", fake.Calls)
	}
	if contains(worktreeCall.Args, "HEAD") {
		t.Errorf("worktree add must pin to the resolved oid, not the literal HEAD: %v", worktreeCall.Args)
	}
	last := worktreeCall.Args[len(worktreeCall.Args)-1]
	if last != localHeadOid {
		t.Errorf("worktree add ref = %q, want resolved oid %q", last, localHeadOid)
	}
}

func TestPrepareLocal_BreadcrumbRoundTripsThroughParseRef(t *testing.T) {
	oids := []string{
		localHeadOid,
		"0000001234567890abcdef", // low-value hex prefix, still nonzero
		"ffffffffffffffffffffff", // max hex prefix
		"abc",                    // shorter than 6/7 chars
	}
	for _, oid := range oids {
		ref := localRef(oid)
		if ref.Number <= 0 {
			t.Errorf("localRef(%q).Number = %d, want > 0", oid, ref.Number)
		}
		got, err := ParseRef(ref.String())
		if err != nil {
			t.Errorf("ParseRef(%q) failed to round-trip: %v", ref.String(), err)
			continue
		}
		if got != ref {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got, ref)
		}
	}
}

func TestPrepareLocal_FindingsDirOutsideWorkspace(t *testing.T) {
	fake := localGitRunner()
	c := testClient(t, fake)
	path := t.TempDir()

	sess, err := c.PrepareLocal(context.Background(), path, PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(sess.Workspace)
		os.RemoveAll(sess.FindingsDir)
	})

	if sandbox.WithinWorkspace(sess.Workspace, sess.FindingsDir) {
		t.Errorf("findings dir %q must be a sibling of workspace %q, not nested inside it", sess.FindingsDir, sess.Workspace)
	}
}

func TestPrepareLocal_FindingsDirIsDurable(t *testing.T) {
	fake := localGitRunner()
	c := testClient(t, fake)
	path := t.TempDir()

	sess, err := c.PrepareLocal(context.Background(), path, PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(sess.Workspace)
		os.RemoveAll(sess.FindingsDir)
	})

	if got, want := filepath.Dir(sess.FindingsDir), c.FindingsDir(); got != want {
		t.Errorf("findings dir parent = %q, want the client's durable findingsDir %q", got, want)
	}
	if !strings.HasPrefix(filepath.Base(sess.FindingsDir), findingsDirPrefix) {
		t.Errorf("findings dir %q lacks the %q prefix", sess.FindingsDir, findingsDirPrefix)
	}
}

func TestLocalProfile_DeniesAllNetworkCLI(t *testing.T) {
	perms := localProfile("/tmp/forgectl-findings-test")
	if !contains(perms.Deny, "Bash(gh:*)") {
		t.Error("deny list must block every gh subcommand via Bash(gh:*)")
	}
	for _, a := range perms.Allow {
		if strings.Contains(a, "gh") {
			t.Errorf("allow list must grant no gh entries at all; found %q", a)
		}
	}
	// rg's --pre flag executes an arbitrary program per searched file — a real
	// command-execution primitive PR mode accepts behind its approval gate.
	// Local mode has no such gate, so it must never grant rg.
	if contains(perms.Allow, "Bash(rg:*)") {
		t.Error("local allow list must not grant Bash(rg:*): rg --pre executes arbitrary commands and local mode has no approval-gate backstop")
	}
}

func TestLocalProfile_FindingsDirIsOnlyWritablePath(t *testing.T) {
	const dir = "/tmp/forgectl-findings-test"
	perms := localProfile(dir)

	var writeGrants []string
	for _, a := range perms.Allow {
		if strings.HasPrefix(a, "Write") {
			writeGrants = append(writeGrants, a)
		}
	}
	if len(writeGrants) != 1 {
		t.Fatalf("expected exactly one Write(...) grant, got %v", writeGrants)
	}
	want := "Write(" + dir + "/**)"
	if writeGrants[0] != want {
		t.Errorf("Write grant = %q, want %q", writeGrants[0], want)
	}
	if contains(perms.Deny, "Write") {
		t.Error(`deny list must not contain bare "Write" — it would clobber the scoped Write(findingsDir/**) allow grant`)
	}
}
