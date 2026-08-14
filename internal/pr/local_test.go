package pr

// Test plan for local.go
//
// PrepareLocal (Classification: ops layer, offline clean-room — no gh, no network)
//   [x] Dry-run: only the two local, read-only `git rev-parse` calls happen;
//       no worktree, no quarantine/allowlist write, no breadcrumb
//   [x] Real: uses `git worktree add`, never `git clone`
//   [x] Real: the worktree ref is the resolved HEAD oid, not the literal "HEAD"
//   [x] newLocalRef output round-trips through ParseRef (the Number<=0 failure mode)
//   [x] The breadcrumb carries "local": true, and loadSession restores IsLocal()
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
	"time"

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
	if !sess.Ref.IsLocal() {
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
		ref := newLocalRef(oid)
		if ref.Number <= 0 {
			t.Errorf("newLocalRef(%q).Number = %d, want > 0", oid, ref.Number)
		}
		// The breadcrumb reload path, which is the reason the round-trip must work.
		got, err := ParseRef(ref.String())
		if err != nil {
			t.Errorf("ParseRef(%q) failed to round-trip: %v", ref.String(), err)
			continue
		}
		// The ref STRING cannot carry locality — the breadcrumb's own Local
		// field does (see loadSession). So the parse comes back non-local and
		// matches only once locality is re-applied.
		if got.IsLocal() {
			t.Errorf("ParseRef(%q) produced a local Ref; only newLocalRef may", ref.String())
		}
		if got.asLocal() != ref {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got.asLocal(), ref)
		}
	}
}

// TestPrepareLocal_PersistsLocalityFlag is the end-to-end statement of the
// locality-persistence contract, from PrepareLocal's write through
// loadSession's read. The ref string cannot carry locality (owner "local" is
// only a display value), so if the breadcrumb's own flag is not written or not
// re-applied, a reloaded session goes silently NON-local — and PostReview's
// refusal plus launchCodex's sandbox posture both key off exactly that bit.
func TestPrepareLocal_PersistsLocalityFlag(t *testing.T) {
	fake := localGitRunner()
	c := testClient(t, fake)

	sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(sess.Workspace)
		os.RemoveAll(sess.FindingsDir)
	})

	raw, err := os.ReadFile(sess.Path)
	if err != nil {
		t.Fatalf("read breadcrumb: %v", err)
	}
	if !strings.Contains(string(raw), `"local": true`) {
		t.Errorf("breadcrumb does not persist locality:\n%s", raw)
	}

	reloaded, err := c.loadSession(sess.Path)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if !reloaded.Ref.IsLocal() {
		t.Errorf("reloaded session lost its locality: %+v", reloaded.Ref)
	}
	if reloaded.Ref.String() != sess.Ref.String() {
		t.Errorf("reloaded ref = %q, want %q", reloaded.Ref.String(), sess.Ref.String())
	}
}

// TestLoadSession_RealLocalOwnerReloadsNonLocal is the other half: a breadcrumb
// whose REF spells owner "local" but whose Local flag is false must reload
// non-local. That is both a real forge repo (git.sjo.lol/local/tools, issue
// #185) and the shape a pre-upgrade local breadcrumb takes — either way, the
// flag is the only thing that grants locality.
func TestLoadSession_RealLocalOwnerReloadsNonLocal(t *testing.T) {
	// No git runner needed: loadSession reads the breadcrumb and shells out to
	// nothing.
	c := testClient(t, &exec.FakeRunner{})
	ws := fakeWorkspace(t)
	ref := Ref{Owner: "local", Repo: "repo", Number: 5}
	bc := Breadcrumb{Workspace: ws, Ref: ref.String(), Agent: "claude", CreatedAt: time.Now().UTC()}

	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("writeBreadcrumb: %v", err)
	}
	sess, err := c.loadSession(path)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if sess.Ref.IsLocal() {
		t.Error("a breadcrumb without the local flag must reload NON-local, whatever its owner spells")
	}
	// The reloaded session's provenance is resolved by loadSession, so this
	// asserts the whole restore path, not just the predicate: a record whose ref
	// reloads non-local cannot carry a positive claim out of the loader.
	if sess.Provenance == ReviewProvenanceOperatorAuthored {
		t.Error("a breadcrumb without the local flag reloaded as operator-authored")
	}
	if err := CheckAgentForReview("codex", sess.Provenance); err == nil {
		t.Error("the Codex refusal must still apply to a reloaded remote ref owned by \"local\"")
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

// TestPrepareLocal_AllowlistOnlyForHarnessesThatReadIt pins the asymmetry:
// .claude/settings.local.json is a Claude Code control, so a `--agent codex`
// session must not get one. Writing it there would leave a file that looks
// like the Codex reviewer's confinement and enforces nothing.
func TestPrepareLocal_AllowlistOnlyForHarnessesThatReadIt(t *testing.T) {
	for _, tc := range []struct {
		agent string
		want  bool
	}{
		{"claude", true},
		{"", true}, // default → agent A
		{"codex", false},
	} {
		t.Run("agent="+tc.agent, func(t *testing.T) {
			c := testClient(t, localGitRunner())
			// Asserted authorship: this test is about which harness gets an
			// allowlist, so the Codex case must get PAST the #232 provenance
			// gate to say anything about it.
			sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{
				Agent:      tc.agent,
				Provenance: ReviewProvenanceOperatorAuthored,
			})
			if err != nil {
				t.Fatalf("PrepareLocal: %v", err)
			}
			t.Cleanup(func() {
				os.RemoveAll(sess.Workspace)
				os.RemoveAll(sess.FindingsDir)
			})

			_, statErr := os.Stat(filepath.Join(sess.Workspace, ".claude", "settings.local.json"))
			if got := statErr == nil; got != tc.want {
				t.Errorf("allowlist present = %v, want %v (agent %q)", got, tc.want, tc.agent)
			}
		})
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

// TestPrepareLocal_RefusesCleanRoomWorkspace closes the laundering path around
// the Codex refusal: `forgectl pr <ref>` (allowed, dispatches Claude) leaves a
// workspace holding the third party's head, and `pr local --agent codex`
// pointed at that workspace would review the same hostile diff with the
// unconfined reviewer. `pr local` means the operator's own tree.
func TestPrepareLocal_RefusesCleanRoomWorkspace(t *testing.T) {
	cleanRoom, err := os.MkdirTemp("", "forgectl-workflow-*")
	if err != nil {
		t.Fatalf("make fake clean room: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(cleanRoom) })

	for _, tc := range []struct{ name, path string }{
		{"the workspace itself", cleanRoom},
		{"a subdirectory of it", filepath.Join(cleanRoom, "src")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.path, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			fake := localGitRunner()
			c := testClient(t, fake)

			_, err := c.PrepareLocal(context.Background(), tc.path, PrepareLocalOpts{Agent: "codex"})
			if err == nil {
				t.Fatal("PrepareLocal must refuse a path inside a clean-room workspace")
			}
			if !strings.Contains(err.Error(), "clean-room workspace") {
				t.Errorf("unexpected error: %v", err)
			}
			// Behavioral: the refusal precedes every Runner call.
			if len(fake.Calls) != 0 {
				t.Errorf("refusal must precede any git call; got %+v", fake.Calls)
			}
		})
	}
}

// TestPrepareLocal_AllowsOrdinaryPaths is the control — the guard must be
// specific to forgectl's own sandboxes, not a blanket ban on temp dirs (t.TempDir
// itself lives under the OS temp root).
func TestPrepareLocal_AllowsOrdinaryPaths(t *testing.T) {
	c := testClient(t, localGitRunner())
	sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{
		Agent:      "codex",
		DryRun:     true,
		Provenance: ReviewProvenanceOperatorAuthored,
	})
	if err != nil {
		t.Fatalf("an ordinary path must still be reviewable: %v", err)
	}
	if !sess.Ref.IsLocal() {
		t.Errorf("expected a local ref, got %+v", sess.Ref)
	}
}

// TestPrepareLocal_RefusesCleanRoomAcrossTMPDIRChange closes a one-variable
// bypass of the clean-room guard. os.TempDir() reads $TMPDIR at CALL time,
// while the workspace was created under the CREATING process's $TMPDIR — so
//
//	forgectl pr owner/repo#1                      # under /var/folders/…/T/
//	TMPDIR=/tmp forgectl pr local --agent codex /var/folders/…/forgectl-workflow-abc
//
// made the temp-root comparison false and returned before the prefix scan ran.
// The recorded breadcrumb workspace is an absolute path that does not move,
// which is what makes the authoritative half $TMPDIR-independent.
func TestPrepareLocal_RefusesCleanRoomAcrossTMPDIRChange(t *testing.T) {
	// A workspace somewhere the *current* $TMPDIR will not cover.
	elsewhere := t.TempDir()
	workspace := filepath.Join(elsewhere, "forgectl-workflow-abc123")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	sessionsDir := t.TempDir()
	c := New(localGitRunner(), WithSessionsDir(sessionsDir), WithTmuxSession("forgectl"))

	// Record the breadcrumb the way a real `forgectl pr <ref>` would.
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	if _, err := writeBreadcrumb(sessionsDir, ref, Breadcrumb{
		Workspace: workspace,
		Ref:       ref.String(),
		Agent:     "claude",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write breadcrumb: %v", err)
	}

	// Now point $TMPDIR somewhere else entirely, as the bypass did.
	t.Setenv("TMPDIR", t.TempDir())

	for _, tc := range []struct{ name, path string }{
		{"the workspace itself", workspace},
		{"a subdirectory of it", filepath.Join(workspace, "src")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.path, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			_, err := c.PrepareLocal(context.Background(), tc.path, PrepareLocalOpts{Agent: "codex"})
			if err == nil {
				t.Fatal("a recorded clean-room workspace must be refused regardless of $TMPDIR")
			}
			if !strings.Contains(err.Error(), "clean-room workspace") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
