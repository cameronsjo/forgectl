package pr

// Test plan for summary.go + List/Attach/Open (#212)
//
// Client.List (Classification: presentation rows, invalid skipped)
//   [x] Live and stale records are listed TOGETHER, sorted newest first
//   [x] An invalid record (existing directory, not a sandbox) is skipped
//   [x] A stale row carries ref, breadcrumb path, and recorded timestamp
// SessionSummary (Classification: private-state presentation API)
//   [x] The predicates are mutually exclusive and both false on the zero value
//   [x] The type exposes no workspace path, agent, or action method
// Attach / Open (Classification: typed remediation, zero Runner calls)
//   [x] A stale breadcrumb refuses with teardown guidance
//   [x] An INVALID breadcrumb refuses WITHOUT teardown guidance
//   [x] Both refuse before any tmux call — including Open's ensureSession

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// seedStaleSession seeds a valid breadcrumb and then deletes its workspace,
// producing exactly the state #212 exists to make recoverable: a well-formed
// record pointing at a lexically absent directory whose parent still exists.
func seedStaleSession(t *testing.T, c *Client, ref Ref, createdAt time.Time) (bcPath, workspace string) {
	t.Helper()
	bcPath, workspace = seedSession(t, c, ref, createdAt)
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatalf("remove workspace to stale the breadcrumb: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace %q should be gone; Stat err = %v", workspace, err)
	}
	return bcPath, workspace
}

// seedInvalidSession writes a record whose workspace EXISTS but is not a
// sandbox — neither live nor a clean absence.
func seedInvalidSession(t *testing.T, c *Client, ref Ref, createdAt time.Time) string {
	t.Helper()
	path, err := writeBreadcrumb(c.SessionsDir(), ref, Breadcrumb{
		Workspace: t.TempDir(),
		Ref:       ref.String(),
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("seed invalid breadcrumb: %v", err)
	}
	return path
}

// TestList_IncludesStaleBreadcrumbs pins the visibility half of the defect: a
// stale record must be LISTED, because a row a user cannot see is a row they
// cannot tear down. An invalid record is still skipped.
func TestList_IncludesStaleBreadcrumbs(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	newest := time.Now().UTC()
	older := newest.Add(-time.Hour)

	liveRef := Ref{Owner: "o", Repo: "r", Number: 1}
	staleRef := Ref{Owner: "o", Repo: "r", Number: 2}
	seedSession(t, c, liveRef, older)
	stalePath, _ := seedStaleSession(t, c, staleRef, newest)
	seedInvalidSession(t, c, Ref{Owner: "o", Repo: "r", Number: 3}, newest)

	got, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d rows, want 2 (one live + one stale; the invalid record is skipped)", len(got))
	}

	// Newest first, live and stale interleaved by time rather than segregated.
	if got[0].Ref() != staleRef || got[1].Ref() != liveRef {
		t.Fatalf("rows = [%s, %s], want newest-first [%s, %s]",
			got[0].Ref(), got[1].Ref(), staleRef, liveRef)
	}
	if !got[0].IsWorkspaceMissing() || got[0].IsWorkspaceLive() {
		t.Error("the stale row must report missing and not live")
	}
	if !got[1].IsWorkspaceLive() || got[1].IsWorkspaceMissing() {
		t.Error("the live row must report live and not missing")
	}
	if got[0].Path() != stalePath {
		t.Errorf("stale row path = %q, want %q — this is the operand teardown takes", got[0].Path(), stalePath)
	}
	if !got[0].CreatedAt().Equal(newest) {
		t.Errorf("stale row createdAt = %v, want the recorded %v", got[0].CreatedAt(), newest)
	}
}

// TestSessionSummary_ZeroValueIsNeitherLiveNorMissing pins the fail-closed
// shape consumers switch on: an unclassified summary must not read as
// actionable, so a consumer's default branch is reachable only for a summary
// that never came from List.
func TestSessionSummary_ZeroValueIsNeitherLiveNorMissing(t *testing.T) {
	var zero SessionSummary
	if zero.IsWorkspaceLive() || zero.IsWorkspaceMissing() {
		t.Errorf("zero SessionSummary reports live=%v missing=%v, want both false",
			zero.IsWorkspaceLive(), zero.IsWorkspaceMissing())
	}
}

func TestAttachOpen_StaleBreadcrumbRefusesWithRemediation(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 7}
	verbs := map[string]func(*Client, string) error{
		"Attach": func(c *Client, p string) error { return c.Attach(context.Background(), p) },
		"Open":   func(c *Client, p string) error { return c.Open(context.Background(), p) },
	}
	for name, call := range verbs {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			c := testClient(t, fake)
			path, _ := seedStaleSession(t, c, ref, time.Now().UTC())

			err := call(c, path)
			if err == nil {
				t.Fatalf("%s on a stale breadcrumb must refuse", name)
			}
			if !strings.Contains(err.Error(), "pr teardown") {
				t.Errorf("%s error must point at teardown remediation, got: %v", name, err)
			}
			// Refusal precedes ensureSession, window targeting, and every
			// tmux call — Open's ensureSession is the one that would
			// otherwise run before the failure was noticed.
			if len(fake.Calls) != 0 {
				t.Errorf("%s must refuse with ZERO Runner calls; got %+v", name, fake.Calls)
			}
			if _, statErr := os.Stat(path); statErr != nil {
				t.Errorf("a refusal must not remove the breadcrumb: %v", statErr)
			}
		})
	}
}

// TestAttachOpen_InvalidBreadcrumbRefusesWithoutRemediation is the other half
// of the typed gate: teardown cannot resolve an invalid record either, so
// advising it there would send the user at a deletion the teardown path is
// going to refuse anyway.
func TestAttachOpen_InvalidBreadcrumbRefusesWithoutRemediation(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 8}
	verbs := map[string]func(*Client, string) error{
		"Attach": func(c *Client, p string) error { return c.Attach(context.Background(), p) },
		"Open":   func(c *Client, p string) error { return c.Open(context.Background(), p) },
	}
	for name, call := range verbs {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			c := testClient(t, fake)
			path := seedInvalidSession(t, c, ref, time.Now().UTC())

			err := call(c, path)
			if err == nil {
				t.Fatalf("%s on an invalid breadcrumb must refuse", name)
			}
			if strings.Contains(err.Error(), "pr teardown") {
				t.Errorf("%s must NOT offer teardown remediation for an invalid record, got: %v", name, err)
			}
			if len(fake.Calls) != 0 {
				t.Errorf("%s must refuse with ZERO Runner calls; got %+v", name, fake.Calls)
			}
		})
	}
}
