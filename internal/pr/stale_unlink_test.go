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
//   [x] Workspace reappeared -> refuse, member intact
//   [x] Every refusal leaves breadcrumb, sibling, parent, and external canaries
//       intact and issues ZERO Runner calls
//   [x] Concurrent same-client teardowns serialize: exactly one unlink succeeds

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		"member replaced with identical bytes": func(t *testing.T, f staleFixture) expect {
			body, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("read member: %v", err)
			}
			if err := os.Remove(f.path); err != nil {
				t.Fatalf("remove member: %v", err)
			}
			if err := os.WriteFile(f.path, body, 0o600); err != nil {
				t.Fatalf("recreate member: %v", err)
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
			return expect{breadcrumbSurvives: false, canariesInPlace: false}
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
