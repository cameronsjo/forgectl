package pr

// Test plan for admission.go
//
// LiveReviews (Classification: fail-safe tmux window count, ListWindows-only)
//   [x] Happy: 2 "pr-*" windows + 1 unrelated window, all in the client's
//       session → counts only the 2 prefixed windows
//   [x] Unhappy: list-windows errors (not "no server running") → (0, false) —
//       the caller MUST treat this as unreadable, never as a genuine zero
//   [x] Boundary: "pr-*" windows that exist in a DIFFERENT session are not
//       counted
//   [x] Happy: no windows at all (including "no server running") → genuine
//       zero, ok=true
//   [x] REGRESSION (fixes the fail-open bug the prior HasSession-gated
//       implementation had): a broken/missing tmux binary makes list-windows
//       itself error → (0, false), NOT a silently-trusted (0, true). Verified
//       by temporarily reverting to the old HasSession-based body: against
//       that code this exact fixture returns (0, true) — see the admission.go
//       doc comment for the full before/after trace.
//   [x] CLOSED (was a documented residual): a sibling session whose name
//       prefixes the client's tmux session (e.g. "forgectl-review" when the
//       client watches "forgectl") no longer swallows a launch — Launch's
//       own new-window now targets the exact-qualified "=name:" form
//       (launch.go), so the window lands in and is counted under the real
//       session. Proven end-to-end against a stateful tmux simulation, not
//       just LiveReviews in isolation — see fakeTmuxServer and
//       TestLiveReviews_SiblingPrefixSession_Closed.
//
// WindowLive / WindowsLive (Classification: fail-SOFT liveness read)
//   [x] Happy: a window dispatched through the real new-window sequence reads
//       live; killing it (what tmux does when the agent exits and
//       remain-on-exit is off) flips it to (false, true) — forgectl#242
//   [x] Unhappy: list-windows errors → (_, false), NOT (false, true). The
//       caller renders "?" from this; collapsing it to "gone" would flag every
//       healthy review as dead whenever tmux hiccups
//   [x] Boundary: a same-named window in a DIFFERENT session is not this
//       client's review
//   [x] Happy: WindowsLive over a mixed set answers each ref correctly from
//       exactly ONE list-windows invocation
//   [x] Unhappy: WindowsLive on an error returns a nil map, never a map of
//       falses (which would read as "all gone")
//
// Admit (Classification: concurrency gate, fail-closed, single resolution)
//   [x] Happy: no windows yet → genuine zero, full cap free, resolved max and
//       live both reported
//   [x] Boundary: live count already at/above max → 0 free, still ok
//   [x] Boundary: a non-positive cfgMax resolves to DefaultMaxConcurrentReviews
//       (Admit is now the ONLY place that resolves cfgMax — callers pass the
//       raw config value through)

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// winRow builds one generation-qualified list-windows -a fixture line, with the
// parent session id windowFormat now carries.
func winRow(session, name string) string {
	return strings.Join([]string{"123", "456", "@1", "$1", session, "1", name, "0", "1"}, "\x1f")
}

// listWindowsFake fakes only `tmux list-windows -a` with rows; every other
// tmux call succeeds as a no-op.
func listWindowsFake(rows ...string) *exec.FakeRunner {
	out := strings.Join(rows, "\n")
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return out, nil
		}
		return "", nil
	}}
}

func TestLiveReviews_CountsPrefixedWindowsInSession(t *testing.T) {
	fake := listWindowsFake(
		winRow("forgectl", "pr-a-b-1"),
		winRow("forgectl", "pr-c-d-2"),
		winRow("forgectl", "shell"),
	)
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 2 {
		t.Fatalf("LiveReviews() = (%d, %v), want (2, true)", n, ok)
	}
}

func TestLiveReviews_ListWindowsErrors(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			// A plain error is never enough to prove an absent default socket.
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, false)", n, ok)
	}
}

func TestLiveReviews_IgnoresOtherSessions(t *testing.T) {
	fake := listWindowsFake(
		winRow("other-session", "pr-a-b-1"),
		winRow("other-session", "pr-c-d-2"),
	)
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, true)", n, ok)
	}
}

func TestLiveReviews_NoWindows_GenuineZero(t *testing.T) {
	fake := listWindowsFake() // empty output — no server, or a server with nothing live
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, true)", n, ok)
	}
}

// TestLiveReviews_BrokenTmux_FailsClosed is the actual behavioral regression
// this file's admission.go change produces: a broken/missing tmux binary now
// surfaces as unreadable (ok=false) rather than a trusted genuine zero. Under
// the PRIOR HasSession-gated implementation this exact fixture returned
// (0, true) — HasSession's own Run() error was swallowed into "session
// absent," short-circuiting before ListWindows ever ran, so a caller admitted
// the full cap and PrepareMany cloned every selected PR before Launch's own
// tmux call finally failed. Verified by hand against the reverted body; see
// the LiveReviews doc comment for the trace.
func TestLiveReviews_BrokenTmux_FailsClosed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", errors.New(`exec: "tmux": executable file not found in $PATH`)
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, false) — a broken tmux binary must fail closed, not read as a trusted zero", n, ok)
	}
}

// fakeTmuxServer is a minimal, STATEFUL simulation of real tmux -t target
// resolution — live-verified on an isolated tmux socket (tmux 3.7b):
//   - -t "$N" / "$N:"  → native id, resolved by identity, no fallback ever
//   - -t "=name:"      → exact match ONLY (the trailing colon is load-bearing —
//     "=name" alone, without it, still prefix-matches for new-window)
//   - -t "name"        → exact match, else PREFIX match against any existing
//     session (tmux's own exact → fnmatch → prefix fallback, collapsed to
//     exact-or-prefix, which is sufficient for these fixtures)
//
// The fuzzy branches are retained deliberately even though forgectl no longer
// emits a name target: they are what makes a regression VISIBLE. If a future
// change hands tmux a bare name again, the simulation resolves it the way real
// tmux does — into the sibling — and the assertions below fail.
//
// It tracks sessions, their native ids, and their windows, so a test can drive
// the real ensureSession/new-window sequence and read back which session
// actually gained the window — the only way to prove a launch does or does not
// land in a sibling without a live tmux.
type fakeTmuxServer struct {
	sessions map[string][]string // session name -> ordered window names
	ids      map[string]string   // session name -> native "$N"
	nextID   int
}

func newFakeTmuxServer(seed map[string][]string) *fakeTmuxServer {
	s := &fakeTmuxServer{sessions: map[string][]string{}, ids: map[string]string{}}
	names := make([]string, 0, len(seed))
	for name := range seed {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic id assignment
	for _, name := range names {
		cp := make([]string, len(seed[name]))
		copy(cp, seed[name])
		s.sessions[name] = cp
		s.ids[name] = s.mintID()
	}
	return s
}

func (s *fakeTmuxServer) mintID() string {
	id := "$" + strconv.Itoa(s.nextID)
	s.nextID++
	return id
}

func (s *fakeTmuxServer) nameForID(id string) (string, bool) {
	for name, got := range s.ids {
		if got == id {
			return name, true
		}
	}
	return "", false
}

func (s *fakeTmuxServer) resolveSession(target string) (string, bool) {
	// A native id resolves by identity and never falls back — that is the whole
	// property #237 buys.
	if strings.HasPrefix(target, "$") {
		return s.nameForID(strings.TrimSuffix(target, ":"))
	}
	exact := strings.HasPrefix(target, "=")
	name := strings.TrimPrefix(target, "=")
	if _, ok := s.sessions[name]; ok {
		return name, true
	}
	if exact {
		return "", false
	}
	names := make([]string, 0, len(s.sessions))
	for sess := range s.sessions {
		names = append(names, sess)
	}
	sort.Strings(names) // deterministic fallback among multiple prefix matches
	for _, sess := range names {
		if strings.HasPrefix(sess, name) {
			return sess, true
		}
	}
	return "", false
}

func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func (s *fakeTmuxServer) runner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "tmux" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			names := make([]string, 0, len(s.sessions))
			for sess := range s.sessions {
				names = append(names, sess)
			}
			sort.Strings(names)
			var rows []string
			for _, sess := range names {
				rows = append(rows, strings.Join([]string{
					"123", "456", s.ids[sess], sess,
					strconv.Itoa(len(s.sessions[sess])), "0", "1700000000", "/w",
				}, "\x1f"))
			}
			return strings.Join(rows, "\n"), nil
		case "new-session":
			newName, _ := argAfter(args, "-s")
			if newName == "" {
				return "", errors.New("new-session: no -s name")
			}
			if _, exists := s.sessions[newName]; exists {
				return "", errors.New("duplicate session: " + newName)
			}
			s.sessions[newName] = []string{"shell"}
			s.ids[newName] = s.mintID()
			return strings.Join([]string{"123", "456", s.ids[newName]}, "\x1f"), nil
		case "new-window":
			target, _ := argAfter(args, "-t")
			winName, _ := argAfter(args, "-n")
			resolveTarget := target
			if strings.HasPrefix(target, "=") && strings.HasSuffix(target, ":") {
				resolveTarget = strings.TrimSuffix(target, ":")
			}
			sess, ok := s.resolveSession(resolveTarget)
			if !ok {
				return "", errors.New("can't find session: " + strings.TrimSuffix(strings.TrimPrefix(target, "="), ":"))
			}
			s.sessions[sess] = append(s.sessions[sess], winName)
			return "", nil
		case "list-windows":
			names := make([]string, 0, len(s.sessions))
			for sess := range s.sessions {
				names = append(names, sess)
			}
			sort.Strings(names)
			var rows []string
			// Window ids are unique across the SERVER, not per session — real
			// tmux mints them from one counter, and resolution by @id would be
			// meaningless if two sessions could both hold @0.
			nextWindowID := 0
			for _, sess := range names {
				for i, w := range s.sessions[sess] {
					rows = append(rows, strings.Join([]string{
						"123", "456", "@" + strconv.Itoa(nextWindowID), s.ids[sess], sess, strconv.Itoa(i), w, "0", "1",
					}, "\x1f"))
					nextWindowID++
				}
			}
			return strings.Join(rows, "\n"), nil
		}
		return "", nil
	}}
}

// TestLiveReviews_SiblingPrefixSession_Closed is the flipped version of what
// was TestLiveReviews_SiblingPrefixSession_KnownResidual: with the exact
// CRITICAL trigger fixture (only "forgectl-review" alive, no literal
// "forgectl"), a launch dispatch must land in the exact "forgectl" session,
// never the sibling — proven by running the SAME ensureSession + new-window
// sequence Launch itself issues against a tmux simulation that reproduces real
// tmux's exact-vs-fuzzy -t resolution (verified live, see fakeTmuxServer's doc
// comment), then reading the count back through LiveReviews.
//
// Since #237 the dispatch target is the review session's native id, so the
// simulation's prefix branch is never reached. That branch is still there, and
// still resolves `forgectl` to `forgectl-review` the way tmux does — so if a
// future change hands tmux a name again, this test fails exactly as it did for
// the original bug.
func TestLiveReviews_SiblingPrefixSession_Closed(t *testing.T) {
	srv := newFakeTmuxServer(map[string][]string{"forgectl-review": {"shell"}})
	c := New(srv.runner(), WithTmuxSession("forgectl"))
	ctx := context.Background()

	// The exact sequence launchInline/launchCodex/Open issue before dispatch.
	session, err := c.ensureSession(ctx)
	if err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	target, err := newWindowTarget(session)
	if err != nil {
		t.Fatalf("newWindowTarget: %v", err)
	}
	if _, err := c.run.Run(ctx, "tmux", "new-window",
		"-t", target, "-n", "pr-o-r-1", "-c", "/tmp"); err != nil {
		t.Fatalf("new-window: %v", err)
	}

	if got := srv.sessions["forgectl-review"]; len(got) != 1 {
		t.Errorf("sibling session forgectl-review must NOT gain a window; has %v", got)
	}
	n, ok := c.LiveReviews(ctx)
	if !ok || n != 1 {
		t.Fatalf("LiveReviews() = (%d, %v), want (1, true) — the review must land in and be counted "+
			"under the exact \"forgectl\" session, not the sibling \"forgectl-review\"", n, ok)
	}
}

// killWindow removes name from session, simulating what real tmux does when a
// window's child process exits and remain-on-exit is off (the default): the
// window is DESTROYED, taking any error the child printed with it. This is the
// exact transition forgectl#242 is about — `tmux new-window` returned 0, the
// breadcrumb landed on disk, and seconds later the window vanished.
func (s *fakeTmuxServer) killWindow(session, name string) bool {
	wins, ok := s.sessions[session]
	if !ok {
		return false
	}
	for i, w := range wins {
		if w == name {
			s.sessions[session] = append(append([]string{}, wins[:i]...), wins[i+1:]...)
			return true
		}
	}
	return false
}

// TestWindowLive_CreatedThenRemoved drives the real dispatch sequence against
// the stateful tmux simulation, then kills the window the way a rejected model
// would, and asserts WindowLive flips from live to gone WITHOUT ever reporting
// unreadable — the whole point being that a dead review is distinguishable
// from a healthy one, and both are distinguishable from a broken tmux.
func TestWindowLive_CreatedThenRemoved(t *testing.T) {
	srv := newFakeTmuxServer(map[string][]string{"forgectl": {"shell"}})
	c := New(srv.runner(), WithTmuxSession("forgectl"))
	ctx := context.Background()
	ref := Ref{Owner: "o", Repo: "r", Number: 1}

	session, err := c.ensureSession(ctx)
	if err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	target, err := newWindowTarget(session)
	if err != nil {
		t.Fatalf("newWindowTarget: %v", err)
	}
	if _, err := c.run.Run(ctx, "tmux", "new-window",
		"-t", target, "-n", mustWindowName(t, ref), "-c", "/tmp"); err != nil {
		t.Fatalf("new-window: %v", err)
	}
	if live, ok := c.WindowLive(ctx, ref); !ok || !live {
		t.Fatalf("WindowLive() right after dispatch = (%v, %v), want (true, true)", live, ok)
	}

	if !srv.killWindow("forgectl", mustWindowName(t, ref)) {
		t.Fatalf("killWindow did not find %q", mustWindowName(t, ref))
	}
	if live, ok := c.WindowLive(ctx, ref); !ok || live {
		t.Fatalf("WindowLive() after the window died = (%v, %v), want (false, true) — a vanished "+
			"window is a readable fact, not an unreadable tmux", live, ok)
	}
}

func TestWindowLive_ListWindowsErrors_ReportsUnreadable(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			// A plain error is never enough to prove an absent default socket.
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	live, ok := c.WindowLive(context.Background(), Ref{Owner: "o", Repo: "r", Number: 1})
	if ok || live {
		t.Fatalf("WindowLive() = (%v, %v), want (false, false) — an unreadable tmux must not "+
			"masquerade as a vanished window", live, ok)
	}
}

func TestWindowLive_IgnoresOtherSessions(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	fake := listWindowsFake(winRow("other-session", mustWindowName(t, ref)))
	c := New(fake, WithTmuxSession("forgectl"))
	if live, ok := c.WindowLive(context.Background(), ref); !ok || live {
		t.Fatalf("WindowLive() = (%v, %v), want (false, true) — a same-named window in another "+
			"session is not this client's review", live, ok)
	}
}

// TestWindowsLive_MixedSet_OneListWindowsCall pins both halves of the batch
// contract: the right answer per ref, from a SINGLE tmux invocation. `pr list`
// calls this once for every breadcrumb on disk, so a per-ref probe would fork
// a tmux process per session.
func TestWindowsLive_MixedSet_OneListWindowsCall(t *testing.T) {
	liveRef := Ref{Owner: "o", Repo: "r", Number: 1}
	goneRef := Ref{Owner: "o", Repo: "r", Number: 2}
	otherSessionRef := Ref{Owner: "o", Repo: "r", Number: 3}

	fake := listWindowsFake(
		winRow("forgectl", mustWindowName(t, liveRef)),
		winRow("forgectl", "shell"),
		winRow("other-session", mustWindowName(t, otherSessionRef)),
	)
	c := New(fake, WithTmuxSession("forgectl"))

	got, ok := c.WindowsLive(context.Background(), []Ref{liveRef, goneRef, otherSessionRef})
	if !ok {
		t.Fatal("WindowsLive() ok = false, want true")
	}
	want := map[Ref]bool{liveRef: true, goneRef: false, otherSessionRef: false}
	for ref, wantLive := range want {
		if got[ref] != wantLive {
			t.Errorf("WindowsLive()[%s] = %v, want %v", ref, got[ref], wantLive)
		}
	}
	if len(got) != len(want) {
		t.Errorf("WindowsLive() returned %d entries, want %d", len(got), len(want))
	}

	calls := 0
	for _, call := range fake.Calls {
		if call.Name == "tmux" && len(call.Args) > 0 && call.Args[0] == "list-windows" {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("list-windows invoked %d times for 3 refs, want exactly 1", calls)
	}
}

func TestWindowsLive_ListWindowsErrors_NilMap(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	got, ok := c.WindowsLive(context.Background(), []Ref{{Owner: "o", Repo: "r", Number: 1}})
	if ok {
		t.Fatal("WindowsLive() ok = true on a list-windows error, want false")
	}
	if got != nil {
		t.Errorf("WindowsLive() map = %v, want nil — a map of falses would read as \"all gone\"", got)
	}
}

func TestAdmit_NoWindowsYet_FullCapFree(t *testing.T) {
	fake := listWindowsFake()
	c := New(fake, WithTmuxSession("forgectl"))
	max, live, free, ok := c.Admit(context.Background(), 4)
	if !ok || max != 4 || live != 0 || free != 4 {
		t.Fatalf("Admit(ctx, 4) = (max=%d, live=%d, free=%d, ok=%v), want (4, 0, 4, true)", max, live, free, ok)
	}
}

func TestAdmit_LiveAtOrAboveMax(t *testing.T) {
	fake := listWindowsFake(
		winRow("forgectl", "pr-a-b-1"),
		winRow("forgectl", "pr-c-d-2"),
	)
	c := New(fake, WithTmuxSession("forgectl"))
	max, live, free, ok := c.Admit(context.Background(), 2)
	if !ok || max != 2 || live != 2 || free != 0 {
		t.Fatalf("Admit(ctx, 2) = (max=%d, live=%d, free=%d, ok=%v), want (2, 2, 0, true)", max, live, free, ok)
	}
}

func TestAdmit_NonPositiveCfgMaxDefaults(t *testing.T) {
	fake := listWindowsFake()
	c := New(fake, WithTmuxSession("forgectl"))
	for _, cfgMax := range []int{0, -1, -100} {
		max, live, free, ok := c.Admit(context.Background(), cfgMax)
		if !ok || max != DefaultMaxConcurrentReviews || live != 0 || free != DefaultMaxConcurrentReviews {
			t.Errorf("Admit(ctx, %d) = (max=%d, live=%d, free=%d, ok=%v), want (%d, 0, %d, true)",
				cfgMax, max, live, free, ok, DefaultMaxConcurrentReviews, DefaultMaxConcurrentReviews)
		}
	}
}

func TestAdmit_ListWindowsErrors_FailsClosed(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	max, live, free, ok := c.Admit(context.Background(), 4)
	if ok || live != 0 || free != 0 || max != 4 {
		t.Fatalf("Admit(ctx, 4) = (max=%d, live=%d, free=%d, ok=%v), want (4, 0, 0, false)", max, live, free, ok)
	}
}
