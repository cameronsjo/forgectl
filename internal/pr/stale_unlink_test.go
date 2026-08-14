package pr

// Test plan for discardStale (#212) — the one path that deletes a file on the
// strength of a record rather than a live sandbox.
//
// Drift between check and act is staged for REAL: each test resolves the
// member (capturing the snapshot), mutates the filesystem, then calls
// discardStale with that snapshot. No production test hook exists, and none is
// needed — the two halves of the protocol are separately callable.
//
// discardStale (Classification: destructive, fail-closed)
//   [x] Happy: removes ONLY the authorized breadcrumb
//   [x] Member replaced with identical bytes (new inode) -> refuse
//   [x] Member replaced by a symlink -> refuse
//   [x] Member bytes changed -> refuse
//   [x] Member security field changed -> refuse
//   [x] Member disappeared -> refuse (never reported as success)
//   [x] Session directory swapped -> refuse
//   [x] Workspace reappeared -> refuse, member intact, and the RENDERED
//       refusal carries no formatting artifact (Live classifies with a nil
//       cause, so this message must not wrap one)
//   [x] Every refusal leaves breadcrumb, sibling, parent, and external canaries
//       intact and issues ZERO Runner calls
//   [x] An absent, UNPREFIXED recorded workspace classifies MISSING and the
//       unlink still touches nothing but the breadcrumb (the prefix rule is
//       undefined without a resolvable path; containment is the guarantee)
//   [x] Concurrent same-client teardowns serialize: exactly one unlink succeeds
//   [x] A LIVE teardown that fails mid-flight never falls back to the stale
//       unlink: the error surfaces and both breadcrumb and workspace survive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// marshalBreadcrumb renders a record exactly as writeBreadcrumb does, so a
// drift case can replace a member with a well-formed but DIFFERENT record.
func marshalBreadcrumb(bc Breadcrumb) ([]byte, error) {
	data, err := json.MarshalIndent(bc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// sameFileAt reports whether the file now at path is the same object discardStale
// captured, by the SAME comparison discardStale makes. A drift case that means to
// exercise an identity refusal asserts through this so it cannot silently degrade
// into asserting nothing when a filesystem recycles the inode number.
func sameFileAt(t *testing.T, path string, want fs.FileInfo) bool {
	t.Helper()
	got, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat the replacement at %s: %v", path, err)
	}
	return os.SameFile(got, want)
}

// staleFixture is one seeded stale breadcrumb plus the snapshot taken over it.
type staleFixture struct {
	client  *Client
	fake    *exec.FakeRunner
	member  breadcrumbMember
	path    string
	ws      string
	canary  canaries
	dirPath string
}

func newStaleFixture(t *testing.T) staleFixture {
	t.Helper()
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	cn := seedCanaries(t, c)
	path, ws := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())

	member, err := c.resolveBreadcrumbMember(path)
	if err != nil {
		t.Fatalf("resolveBreadcrumbMember: %v", err)
	}
	return staleFixture{
		client: c, fake: fake, member: member,
		path: path, ws: ws, canary: cn, dirPath: c.SessionsDir(),
	}
}

func TestDiscardStale_RemovesOnlyTheAuthorizedBreadcrumb(t *testing.T) {
	f := newStaleFixture(t)
	// A file beside the recorded workspace: a stale unlink must not reach into
	// the workspace's parent at all.
	wsNeighbor := filepath.Join(filepath.Dir(f.ws), "neighbor")
	if err := os.WriteFile(wsNeighbor, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed workspace neighbor: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(wsNeighbor) })

	if err := f.client.discardStale(f.member); err != nil {
		t.Fatalf("discardStale: %v", err)
	}
	if _, err := os.Lstat(f.path); !os.IsNotExist(err) {
		t.Errorf("the authorized breadcrumb should be gone; Lstat err = %v", err)
	}
	if body, err := os.ReadFile(wsNeighbor); err != nil || string(body) != "keep" {
		t.Errorf("a stale unlink must not touch the workspace's parent: body=%q err=%v", body, err)
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("a stale unlink must issue ZERO Runner calls; got %+v", f.fake.Calls)
	}
	f.canary.assertIntact(t)
}

func TestDiscardStale_RefusesOnDrift(t *testing.T) {
	// Each case mutates the filesystem AFTER the snapshot was taken, and
	// declares what should still be assertable afterwards. A case that itself
	// removes or relocates the breadcrumb or the whole directory cannot expect
	// to find them where they were.
	type expect struct {
		breadcrumbSurvives bool
		canariesInPlace    bool
	}
	intact := expect{breadcrumbSurvives: true, canariesInPlace: true}

	cases := map[string]func(t *testing.T, f staleFixture) expect{
		// A DIFFERENT file carrying the same bytes at the same name. Identity
		// is dev+ino, and an inode number is only unique among LIVE files: a
		// filesystem is free to hand the just-unlinked number straight back to
		// the next create, which ext4 does and APFS does not. Left to the
		// allocator this case asserts an accident of the host filesystem, so
		// it holds the original open across the unlink — an open descriptor
		// keeps the inode allocated, so the replacement is guaranteed a
		// different one and the member SameFile arm is what has to refuse.
		"member replaced with identical bytes": func(t *testing.T, f staleFixture) expect {
			body, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("read member: %v", err)
			}
			pin, err := os.Open(f.path)
			if err != nil {
				t.Fatalf("pin the original inode: %v", err)
			}
			// Closed only after the subtest calls discardStale.
			t.Cleanup(func() { _ = pin.Close() })
			if err := os.Remove(f.path); err != nil {
				t.Fatalf("remove member: %v", err)
			}
			if err := os.WriteFile(f.path, body, 0o600); err != nil {
				t.Fatalf("recreate member: %v", err)
			}
			if sameFileAt(t, f.path, f.member.info) {
				t.Fatalf("the replacement reused the original inode; this case cannot test what it claims")
			}
			return intact
		},
		"member replaced by a symlink": func(t *testing.T, f staleFixture) expect {
			body, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("read member: %v", err)
			}
			target := filepath.Join(f.dirPath, "swapped-target.json")
			if err := os.WriteFile(target, body, 0o600); err != nil {
				t.Fatalf("seed swap target: %v", err)
			}
			if err := os.Remove(f.path); err != nil {
				t.Fatalf("remove member: %v", err)
			}
			if err := os.Symlink(target, f.path); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			return intact
		},
		"member bytes changed": func(t *testing.T, f staleFixture) expect {
			body, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("read member: %v", err)
			}
			// Append whitespace: still decodes to the same record, so ONLY the
			// byte check can catch it.
			if err := os.WriteFile(f.path, append(body, ' ', '\n'), 0o600); err != nil {
				t.Fatalf("rewrite member: %v", err)
			}
			return intact
		},
		"member security field changed": func(t *testing.T, f staleFixture) expect {
			other := fakeWorkspace(t)
			if err := os.RemoveAll(other); err != nil {
				t.Fatalf("stale the replacement workspace: %v", err)
			}
			bc := f.member.breadcrumb
			bc.Workspace = other
			body, err := marshalBreadcrumb(bc)
			if err != nil {
				t.Fatalf("marshal replacement record: %v", err)
			}
			if err := os.WriteFile(f.path, body, 0o600); err != nil {
				t.Fatalf("rewrite member: %v", err)
			}
			return intact
		},
		"member disappeared": func(t *testing.T, f staleFixture) expect {
			if err := os.Remove(f.path); err != nil {
				t.Fatalf("remove member: %v", err)
			}
			// The case removed the breadcrumb itself; the canaries are
			// untouched and must still be verifiable.
			return expect{breadcrumbSurvives: false, canariesInPlace: true}
		},
		"session directory swapped": func(t *testing.T, f staleFixture) expect {
			moved := f.dirPath + ".moved"
			if err := os.Rename(f.dirPath, moved); err != nil {
				t.Fatalf("move session dir: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(moved) })
			if err := os.MkdirAll(f.dirPath, 0o700); err != nil {
				t.Fatalf("recreate session dir: %v", err)
			}
			// The whole directory moved, so both the breadcrumb and the
			// sibling canary are legitimately absent from their old paths.
			// What matters is that discardStale refuses rather than deleting
			// a same-named file inside a directory it never verified.
			//
			// Note WHICH guard refuses this one: the swapped-in directory is
			// empty, so the re-stat of the member's base name fails first and
			// the directory identity check never gets the chance. The case
			// below is the one that isolates it.
			return expect{breadcrumbSurvives: false, canariesInPlace: false}
		},
		// A hard link to the member, in a directory swapped in under the same
		// pathname, passes every check downstream of the directory identity
		// one: same inode, same bytes, same decoded record, workspace still
		// absent. Only the pinned directory differs — a rename carries the
		// original inode to its new name, so the recreated directory is a new
		// object. Without the identity check the whole suite stays green while
		// discardStale unlinks through a directory nobody verified, which is
		// exactly the swap its comment warns about.
		"session directory swapped for one holding a hard link to the member": func(t *testing.T, f staleFixture) expect {
			moved := f.dirPath + ".moved"
			if err := os.Rename(f.dirPath, moved); err != nil {
				t.Fatalf("move session dir: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(moved) })
			if err := os.MkdirAll(f.dirPath, 0o700); err != nil {
				t.Fatalf("recreate session dir: %v", err)
			}
			if err := os.Link(filepath.Join(moved, filepath.Base(f.path)), f.path); err != nil {
				t.Skipf("hard links unsupported: %v", err)
			}
			// The member is reachable at its original pathname and must
			// survive; the sibling canary went with the moved directory.
			return expect{breadcrumbSurvives: true, canariesInPlace: false}
		},
		"workspace reappeared": func(t *testing.T, f staleFixture) expect {
			if err := os.MkdirAll(f.ws, 0o700); err != nil {
				t.Fatalf("recreate workspace: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(f.ws) })
			return intact
		},
	}

	for name, drift := range cases {
		t.Run(name, func(t *testing.T) {
			f := newStaleFixture(t)
			want := drift(t, f)

			err := f.client.discardStale(f.member)
			if err == nil {
				t.Fatalf("discardStale must refuse after %s", name)
			}
			if want.breadcrumbSurvives {
				if _, statErr := os.Lstat(f.path); statErr != nil {
					t.Errorf("a refusal must leave the breadcrumb in place: %v", statErr)
				}
			}
			if len(f.fake.Calls) != 0 {
				t.Errorf("a refusal must issue ZERO Runner calls; got %+v", f.fake.Calls)
			}
			if want.canariesInPlace {
				f.canary.assertIntact(t)
			}
		})
	}
}

// TestDiscardStale_ReappearedWorkspaceRendersACleanRefusal pins the TEXT of the
// likeliest refusal on this path, not merely that one occurred.
//
// A workspace that came back between classification and the unlink classifies
// LIVE, and classifyWorkspace returns a NIL error for Live. The refusal used to
// wrap that cause unconditionally, so the operator saw "%!w(<nil>)" — a message
// that reads as a forgectl bug at exactly the moment it should read as "your
// workspace is back, nothing was deleted".
func TestDiscardStale_ReappearedWorkspaceRendersACleanRefusal(t *testing.T) {
	f := newStaleFixture(t)
	if err := os.MkdirAll(f.ws, 0o700); err != nil {
		t.Fatalf("recreate workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(f.ws) })

	err := f.client.discardStale(f.member)
	if err == nil {
		t.Fatal("discardStale must refuse once the recorded workspace is live again")
	}
	got := err.Error()
	if strings.Contains(got, "%!") || strings.Contains(got, "<nil>") {
		t.Errorf("the refusal must not render a formatting artifact: %q", got)
	}
	want := fmt.Sprintf(
		"workspace for breadcrumb %s is no longer cleanly absent; refusing to remove it", f.path)
	if got != want {
		t.Errorf("refusal = %q, want %q", got, want)
	}
	if _, statErr := os.Lstat(f.path); statErr != nil {
		t.Errorf("a refusal must leave the breadcrumb in place: %v", statErr)
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("a refusal must issue ZERO Runner calls; got %+v", f.fake.Calls)
	}
}

// TestDiscardStale_UnprefixedRecordedWorkspaceTouchesNothing answers the
// standing question about this branch: classifyWorkspace grants MISSING
// without repeating validateWorkspace's sandbox-prefix rule, so a record
// naming an absent, unprefixed path — /etc/no-such-dir in the general case —
// does authorize a stale unlink.
//
// The prefix rule cannot be repeated here, and should not be approximated.
// It is defined on filepath.EvalSymlinks(workspace), and a path whose final
// component is absent has nothing to resolve; checking the LITERAL base name
// instead would be a different, weaker rule that refuses the supported shape
// TestClassifyWorkspace_LiveThroughSymlink pins — a workspace reached through
// an unprefixed link name. Such a record would become permanently unremovable,
// which is #212 itself.
//
// What actually bounds the damage is that Missing authorizes ONE syscall, and
// it does not name the workspace: discardStale unlinks the member's base name
// through a pinned handle on the 0700 session directory. The recorded
// workspace is never an operand. That is what this test pins — the containment,
// on the hostile shape.
func TestDiscardStale_UnprefixedRecordedWorkspaceTouchesNothing(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)

	// A real directory holding a canary, with the recorded workspace named as
	// an absent, UNPREFIXED child of it. Nothing here may be disturbed.
	parent := t.TempDir()
	canary := filepath.Join(parent, "must-survive")
	if err := os.WriteFile(canary, []byte("intact"), 0o600); err != nil {
		t.Fatalf("seed workspace-side canary: %v", err)
	}
	workspace := filepath.Join(parent, "no-such-dir")

	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, Breadcrumb{
		Workspace: workspace, Ref: ref.String(), Agent: "claude", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed breadcrumb: %v", err)
	}

	// The accepted classification, asserted rather than assumed.
	avail, _ := classifyWorkspace(workspace)
	if avail != workspaceAvailabilityMissing {
		t.Fatalf("availability = %d, want missing — this test exists for that grant", avail)
	}

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown of an unprefixed stale record: %v", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Errorf("the breadcrumb is the only thing removed; Lstat err = %v", statErr)
	}
	body, readErr := os.ReadFile(canary)
	if readErr != nil || string(body) != "intact" {
		t.Errorf("nothing at the recorded workspace's parent may be touched: body=%q err=%v", body, readErr)
	}
	if _, statErr := os.Stat(parent); statErr != nil {
		t.Errorf("the recorded workspace's parent must survive: %v", statErr)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a stale teardown must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}

// symlinkedSessionsDir returns (link, real): a session directory reached
// through a final-component symlink, which is a supported configuration.
func symlinkedSessionsDir(t *testing.T) (link, real string) {
	t.Helper()
	base := t.TempDir()
	real = filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("create real sessions dir: %v", err)
	}
	link = filepath.Join(base, "sessions")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	return link, real
}

// TestDiscardStale_ThroughSymlinkedSessionsDir pins that pinning the directory
// with os.OpenRoot did not break a sessions dir reached through a symlink.
// os.Root refuses an ESCAPING symlink but resolves the root path itself
// normally, so this configuration keeps working.
func TestDiscardStale_ThroughSymlinkedSessionsDir(t *testing.T) {
	fake := &exec.FakeRunner{}
	link, real := symlinkedSessionsDir(t)
	c := testClientAt(t, fake, link)
	path, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())

	member, err := c.resolveBreadcrumbMember(path)
	if err != nil {
		t.Fatalf("resolveBreadcrumbMember: %v", err)
	}
	if err := c.discardStale(member); err != nil {
		t.Fatalf("discardStale through a symlinked sessions dir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(real, filepath.Base(path))); !os.IsNotExist(err) {
		t.Errorf("the breadcrumb should be gone from the real dir; Lstat err = %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a stale unlink must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}

// TestDiscardStale_RefusesWhenSessionsSymlinkIsSwapped stages the attack the
// pinned handle exists to answer: the sessions symlink is repointed at a decoy
// holding an identically-named file after the snapshot is taken. The unlink
// must refuse, and neither directory may lose a file.
func TestDiscardStale_RefusesWhenSessionsSymlinkIsSwapped(t *testing.T) {
	fake := &exec.FakeRunner{}
	link, real := symlinkedSessionsDir(t)
	c := testClientAt(t, fake, link)
	path, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())
	name := filepath.Base(path)

	member, err := c.resolveBreadcrumbMember(path)
	if err != nil {
		t.Fatalf("resolveBreadcrumbMember: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(real, name))
	if err != nil {
		t.Fatalf("read seeded breadcrumb: %v", err)
	}
	decoy := filepath.Join(filepath.Dir(real), "decoy")
	if err := os.Mkdir(decoy, 0o700); err != nil {
		t.Fatalf("create decoy dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decoy, name), body, 0o600); err != nil {
		t.Fatalf("seed decoy breadcrumb: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatalf("drop sessions symlink: %v", err)
	}
	if err := os.Symlink(decoy, link); err != nil {
		t.Fatalf("repoint sessions symlink: %v", err)
	}

	if err := c.discardStale(member); err == nil {
		t.Fatal("discardStale must refuse once the sessions symlink was repointed")
	}
	if _, err := os.Lstat(filepath.Join(real, name)); err != nil {
		t.Errorf("the real breadcrumb must survive: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(decoy, name)); err != nil {
		t.Errorf("the decoy breadcrumb must be untouched: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a refusal must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}

// TestDiscardStale_WorkspaceReappearanceLeavesItUntouched pins that a
// reappeared workspace is not merely a refusal reason but is itself never
// touched — the refusal must not "clean up" the directory it just found.
func TestDiscardStale_WorkspaceReappearanceLeavesItUntouched(t *testing.T) {
	f := newStaleFixture(t)
	if err := os.MkdirAll(f.ws, 0o700); err != nil {
		t.Fatalf("recreate workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(f.ws) })
	inside := filepath.Join(f.ws, "replacement-content")
	if err := os.WriteFile(inside, []byte("mine"), 0o600); err != nil {
		t.Fatalf("seed replacement content: %v", err)
	}

	if err := f.client.discardStale(f.member); err == nil {
		t.Fatal("discardStale must refuse once the workspace is back")
	}
	if body, err := os.ReadFile(inside); err != nil || string(body) != "mine" {
		t.Errorf("the reappeared workspace must be untouched: body=%q err=%v", body, err)
	}
	if _, err := os.Stat(f.path); err != nil {
		t.Errorf("the breadcrumb must survive a refusal: %v", err)
	}
}

// TestTeardown_LiveFailureNeverEntersTheStaleUnlink pins the branch boundary
// documented on Teardown: once the live path has begun mutating, a failure
// must surface as an error rather than degrade into the stale unlink.
//
// What it injects is exactly one thing — sandbox.Teardown returning an error
// while leaving the workspace in place, the shape of a removal blocked by
// permissions. Everything before it (membership, classification, quarantine
// restore) runs for real. It does NOT prove the workspace is what blocked the
// removal; it proves that when workspace removal fails, forgectl deletes
// nothing and reports.
func TestTeardown_LiveFailureNeverEntersTheStaleUnlink(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	path, ws := seedSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())

	injected := errors.New("teardown workspace: permission denied")
	restore := sandboxTeardown
	sandboxTeardown = func(context.Context, exec.Runner, string) error { return injected }
	t.Cleanup(func() { sandboxTeardown = restore })

	err := c.Teardown(context.Background(), path)
	if !errors.Is(err, injected) {
		t.Fatalf("a failed live teardown must return its error, got %v", err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Errorf("the breadcrumb must survive a failed live teardown: %v", statErr)
	}
	if _, statErr := os.Stat(ws); statErr != nil {
		t.Errorf("the workspace must survive a failed live teardown: %v", statErr)
	}
}

// TestTeardown_ConcurrentStaleTeardownsSerialize pins the per-client mutex:
// two teardowns of the same stale breadcrumb must not both unlink, and neither
// may issue a Runner call.
//
// Race-detector green is supplementary here, not proof of pathname safety —
// the honest residual is documented on discardStale.
func TestTeardown_ConcurrentStaleTeardownsSerialize(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	path, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())

	const attempts = 2
	var wg sync.WaitGroup
	errs := make([]error, attempts)
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = c.Teardown(context.Background(), path)
		}()
	}
	close(start)
	wg.Wait()

	var succeeded int
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("exactly one concurrent teardown should succeed, got %d (errs: %v)", succeeded, errs)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the breadcrumb should be gone after the winning teardown; Lstat err = %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("stale teardowns must issue ZERO Runner calls; got %+v", fake.Calls)
	}
}
