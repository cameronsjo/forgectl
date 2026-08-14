package pr

// Provenance threaded through the live pipeline (forgectl#232).
//
// The plan pins TWO distinct mutation boundaries, and they promise different
// things:
//
//  1. At initial preparation, an ineligible Codex request refuses before ANY
//     new workspace, findings dir, allowlist, breadcrumb, or tmux state exists.
//     The two read-only `git rev-parse` probes are pinned to PRECEDE the
//     refusal, so an unborn HEAD still reports its own error.
//  2. At reconstructed Launch, the workspace and breadcrumb ALREADY exist.
//     Refusal guarantees zero NEW launch-time mutations — no tmux session or
//     window, no process — while deliberately preserving prior state so Claude,
//     manage, and teardown keep working. It is not a claim of zero mutation.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// detachedGitRunner is localGitRunner with a DETACHED HEAD — what `gh pr
// checkout` leaves behind, and the exact shape forgectl#232 is about.
func detachedGitRunner() *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" && len(args) >= 3 && args[2] == "rev-parse" {
				if contains(args, "--abbrev-ref") {
					return "HEAD", nil // detached
				}
				return localHeadOid, nil
			}
			return "", nil
		},
	}
}

// revParseCalls counts the read-only HEAD probes, which are the ONLY calls
// permitted to precede a local refusal.
func revParseCalls(calls []exec.Call) int {
	n := 0
	for _, c := range calls {
		if c.Name == "git" && len(c.Args) >= 3 && c.Args[2] == "rev-parse" {
			n++
		}
	}
	return n
}

// TestPrepareLocal_CodexRefusedWithoutAssertion is the core of forgectl#232.
// An ordinary local review — attached or detached — carries NO authorship
// claim, so the unconfined path is refused. Detachedness is not what decides
// it: the absence of an assertion is.
func TestPrepareLocal_CodexRefusedWithoutAssertion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner func() *exec.FakeRunner
	}{
		{"attached", localGitRunner},
		{"detached (gh pr checkout)", detachedGitRunner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.runner()
			c := testClient(t, fake)

			_, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{Agent: "codex"})
			if err == nil {
				t.Fatal("PrepareLocal must refuse Codex without an operator-authored assertion")
			}

			// BOUNDARY 1: the read-only probes may run; nothing else may.
			if n := len(fake.Calls); n != revParseCalls(fake.Calls) {
				t.Errorf("refusal must follow ONLY the read-only HEAD probes; got %+v", fake.Calls)
			}
			// No findings dir was created under the client's durable root.
			entries, err := os.ReadDir(c.FindingsDir())
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read findings root: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("refusal created findings state: %v", entries)
			}
			// No breadcrumb was written.
			bcs, err := os.ReadDir(c.SessionsDir())
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read sessions dir: %v", err)
			}
			if len(bcs) != 0 {
				t.Errorf("refusal wrote a breadcrumb: %v", bcs)
			}
		})
	}
}

// TestPrepareLocal_ClaudeUnaffectedByProvenance is the control that keeps this
// change scoped. Claude is confined by its own allowlist, so every provenance
// state — including a detached HEAD with no assertion — must still prepare.
func TestPrepareLocal_ClaudeUnaffectedByProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner func() *exec.FakeRunner
	}{
		{"attached", localGitRunner},
		{"detached", detachedGitRunner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, tc.runner())
			sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{Agent: "claude"})
			if err != nil {
				t.Fatalf("Claude must remain available in every provenance state: %v", err)
			}
			t.Cleanup(func() {
				_ = os.RemoveAll(sess.Workspace)
				_ = os.RemoveAll(sess.FindingsDir)
			})
			if sess.Provenance != ReviewProvenanceUnknown {
				t.Errorf("undeclared local session provenance = %v, want unknown", sess.Provenance)
			}
		})
	}
}

// TestPrepareLocal_AssertionPermitsCodex is the other direction: the assertion
// is the ONLY thing that opens the path, and it works on a detached HEAD too.
// Detachedness neither upgrades nor downgrades a valid declaration — an
// operator can legitimately be reviewing their own detached commit.
func TestPrepareLocal_AssertionPermitsCodex(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner func() *exec.FakeRunner
	}{
		{"attached", localGitRunner},
		{"detached", detachedGitRunner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, tc.runner())
			sess, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{
				Agent:      "codex",
				Provenance: ReviewProvenanceOperatorAuthored,
			})
			if err != nil {
				t.Fatalf("an asserted local review must permit Codex: %v", err)
			}
			t.Cleanup(func() {
				_ = os.RemoveAll(sess.Workspace)
				_ = os.RemoveAll(sess.FindingsDir)
			})
			if sess.Provenance != ReviewProvenanceOperatorAuthored {
				t.Errorf("Session.Provenance = %v, want operator-authored", sess.Provenance)
			}
			// The intended Codex posture is still in place: findings dir exists,
			// and no Claude allowlist was written for a harness that ignores it.
			if sess.FindingsDir == "" {
				t.Error("asserted Codex local review has no findings dir")
			}
			if _, err := os.Stat(filepath.Join(sess.Workspace, ".claude", "settings.local.json")); err == nil {
				t.Error("a Codex session must not get a Claude allowlist")
			}
		})
	}
}

// TestPrepareLocal_UnbornHeadKeepsRevParseError pins the probe ordering. An
// unborn or invalid HEAD must report git's own error, not a provenance refusal
// — otherwise the assertion flag would change the diagnosis of an unrelated
// failure, and no preparation state may exist either way.
func TestPrepareLocal_UnbornHeadKeepsRevParseError(t *testing.T) {
	for _, provenance := range []ReviewProvenance{
		ReviewProvenanceUnknown,
		ReviewProvenanceOperatorAuthored,
	} {
		fake := &exec.FakeRunner{
			RunFunc: func(name string, args []string) (string, error) {
				if name == "git" && len(args) >= 3 && args[2] == "rev-parse" {
					return "", os.ErrInvalid // unborn HEAD
				}
				return "", nil
			},
		}
		c := testClient(t, fake)
		_, err := c.PrepareLocal(context.Background(), t.TempDir(), PrepareLocalOpts{
			Agent:      "codex",
			Provenance: provenance,
		})
		if err == nil {
			t.Fatalf("provenance=%v: an unborn HEAD must fail", provenance)
		}
		if !strings.Contains(err.Error(), "resolve local HEAD") {
			t.Errorf("provenance=%v: want the rev-parse error, got %v", provenance, err)
		}
	}
}

// TestPrepare_RemoteCannotDeclareAuthorship is plan test 11 at the preparation
// boundary. A remote route that passes an operator-authored declaration — a
// caller bug, or a future route that copies the wrong literal — must not open
// the unconfined path, and must refuse before the `gh pr view` round-trip.
func TestPrepare_RemoteCannotDeclareAuthorship(t *testing.T) {
	fake := ghViewRunner()
	c := testClient(t, fake)
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}

	_, err := c.Prepare(context.Background(), ref, PrepareOpts{
		Agent:      "codex",
		Provenance: ReviewProvenanceOperatorAuthored,
	})
	if err == nil {
		t.Fatal("a remote ref must not be able to declare operator authorship")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("refusal must precede every Runner call; got %+v", fake.Calls)
	}
}

// TestPrepare_RemoteRecordsThirdParty proves the remote route's provenance is
// third-party by construction rather than merely undeclared — so a breadcrumb
// written from it reloads with its origin known, not guessed.
func TestPrepare_RemoteRecordsThirdParty(t *testing.T) {
	c := testClient(t, ghViewRunner())
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}

	sess, err := c.Prepare(context.Background(), ref, PrepareOpts{
		Agent:      "claude",
		Provenance: ReviewProvenanceThirdParty,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sess.Workspace) })
	if sess.Provenance != ReviewProvenanceThirdParty {
		t.Errorf("remote Session.Provenance = %v, want third-party", sess.Provenance)
	}
}

// TestLaunch_ReconstructedIneligibleCodexMakesNoNewMutation is BOUNDARY 2, and
// the authoritative gate. A session rebuilt from a breadcrumb never re-enters
// preparation, so Launch is the only point it passes. Refusal must create no
// tmux session, no window, and no process — while leaving the pre-existing
// workspace and breadcrumb untouched for Claude and teardown.
func TestLaunch_ReconstructedIneligibleCodexMakesNoNewMutation(t *testing.T) {
	for _, provenance := range []ReviewProvenance{
		ReviewProvenanceUnknown,
		ReviewProvenanceThirdParty,
	} {
		fake := &exec.FakeRunner{}
		c := testClient(t, fake)
		ws := fakeWorkspace(t)
		sess := Session{
			Ref:         mustLocalRef("abc1234", 1),
			Workspace:   ws,
			Agent:       "codex",
			FindingsDir: t.TempDir(),
			Provenance:  provenance,
			CreatedAt:   time.Now().UTC(),
		}

		_, err := c.Launch(context.Background(), sess, config.Config{})
		if err == nil {
			t.Fatalf("provenance=%v: Launch must refuse the unconfined path", provenance)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("provenance=%v: refusal made launch-time calls: %+v", provenance, fake.Calls)
		}
		// Prior state is PRESERVED, not cleaned up — Claude and teardown still
		// need it.
		if _, statErr := os.Stat(ws); statErr != nil {
			t.Errorf("provenance=%v: refusal destroyed the pre-existing workspace: %v", provenance, statErr)
		}
	}
}

// TestLaunch_RemoteSessionCannotSelfDeclare is the hostile end-to-end shape:
// even if a Session arrives at Launch carrying an operator-authored value on a
// REMOTE ref, the authoritative re-check downgrades it and refuses.
func TestLaunch_RemoteSessionCannotSelfDeclare(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	sess := Session{
		Ref:        Ref{Owner: "o", Repo: "r", Number: 42},
		Workspace:  fakeWorkspace(t),
		Agent:      "codex",
		Provenance: ReviewProvenanceOperatorAuthored,
		CreatedAt:  time.Now().UTC(),
	}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("a remote session self-declaring authorship reached the unconfined path")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("refusal made launch-time calls: %+v", fake.Calls)
	}
}
