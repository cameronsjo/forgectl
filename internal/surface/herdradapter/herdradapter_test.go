package herdradapter

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

const (
	testSocket    = "/tmp/herdrtest/herdr.sock"
	testHerdr     = "/opt/homebrew/bin/herdr"
	testCWD       = "/Users/example/code/forgectl"
	testBootstrap = "forgectl surface _exec --protocol 1"
	testUID       = 501
	liveInode     = 4242
	// Real herdr id shapes, measured: workspaces are short, tabs and panes are
	// the workspace qualified with a colon.
	wsA   = "w2R"
	wsB   = "w2S"
	tabA  = "w2R:t1"
	paneA = "w2R:p1"
)

type fakeInfo struct {
	sys  any
	mode fs.FileMode
}

func (fakeInfo) Name() string        { return "herdr.sock" }
func (fakeInfo) Size() int64         { return 0 }
func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (fakeInfo) ModTime() time.Time  { return time.Unix(0, 0) }
func (f fakeInfo) IsDir() bool       { return f.mode.IsDir() }
func (f fakeInfo) Sys() any          { return f.sys }

func liveSocket(inode uint64) func(string) (os.FileInfo, error) {
	return func(string) (os.FileInfo, error) {
		return fakeInfo{sys: &syscall.Stat_t{Ino: inode, Uid: testUID}, mode: fs.ModeSocket | 0o600}, nil
	}
}

func absentSocket(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

func sessionsJSON(rows ...map[string]any) []byte {
	raw, err := json.Marshal(map[string]any{"sessions": rows})
	if err != nil {
		panic(err)
	}
	return raw
}

func defaultSessions() []byte {
	return sessionsJSON(map[string]any{
		"default": true, "name": defaultSession, "running": true,
		"session_dir": "/tmp/herdrtest", "socket_path": testSocket,
	})
}

func statusJSON(running, compatible bool, protocol int) []byte {
	raw, err := json.Marshal(map[string]any{
		"server": map[string]any{"running": running, "compatible": compatible, "protocol": protocol},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func goodStatus() []byte { return statusJSON(true, true, minProtocol) }

func createJSON(ws, tab, pane string) []byte {
	raw, err := json.Marshal(map[string]any{
		"id": "cli:workspace:create",
		"result": map[string]any{
			"type":      "workspace_created",
			"workspace": map[string]any{"workspace_id": ws},
			"tab":       map[string]any{"tab_id": tab},
			"root_pane": map[string]any{"pane_id": pane},
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// listJSON renders a workspace listing from id→label pairs.
func listJSON(rows ...[2]string) []byte {
	ws := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		ws = append(ws, map[string]any{"workspace_id": r[0], "label": r[1]})
	}
	raw, err := json.Marshal(map[string]any{
		"id":     "cli:workspace:list",
		"result": map[string]any{"type": "workspace_list", "workspaces": ws},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func errorJSON(code string) []byte {
	raw, err := json.Marshal(map[string]any{
		"id":    "cli:workspace:close",
		"error": map[string]any{"code": code, "message": "prose that may be reworded"},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func stdout(b []byte) exec.SensitiveResult {
	return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest(b, exec.OutputComplete)}
}

// scriptedRunner answers by CommandKind, and — because readiness issues TWO
// commands under one kind — by whether the argv is the session roster.
type scriptedRunner struct {
	fake     exec.FakeSensitiveRunner
	reply    map[exec.CommandKind]func() (exec.SensitiveResult, error)
	sessions func() (exec.SensitiveResult, error)
	create   int
}

func newRunner() *scriptedRunner {
	r := &scriptedRunner{reply: map[exec.CommandKind]func() (exec.SensitiveResult, error){}}
	r.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrReadiness && isSessionList(cmd) {
			if r.sessions != nil {
				return r.sessions()
			}
			return stdout(defaultSessions()), nil
		}
		if fn, ok := r.reply[cmd.Kind]; ok {
			// The create kind covers BOTH the workspace create and the pane run,
			// so a scripted reply must not answer the run with a create body.
			if cmd.Kind == exec.KindHerdrCreate {
				r.create++
				if r.create > 1 {
					return exec.SensitiveResult{}, nil
				}
			}
			return fn()
		}
		switch cmd.Kind {
		case exec.KindHerdrReadiness:
			return stdout(goodStatus()), nil
		case exec.KindHerdrSnapshot, exec.KindHerdrReconcile, exec.KindHerdrProbe:
			return stdout(listJSON()), nil
		case exec.KindHerdrCreate:
			r.create++
			if r.create > 1 {
				return exec.SensitiveResult{}, nil // the pane run
			}
			return stdout(createJSON(wsA, tabA, paneA)), nil
		default:
			return exec.SensitiveResult{}, nil
		}
	}
	return r
}

// isSessionList identifies the roster read, which is the one command that must
// NOT carry the session pin.
func isSessionList(cmd exec.SensitiveCommand) bool {
	for i, a := range cmd.Args {
		if a.Equal(exec.MustFixed("session")) && i+1 < len(cmd.Args) &&
			cmd.Args[i+1].Equal(exec.MustFixed("list")) {
			return true
		}
	}
	return false
}

func (r *scriptedRunner) on(kind exec.CommandKind, fn func() (exec.SensitiveResult, error)) *scriptedRunner {
	r.reply[kind] = fn
	return r
}

func (r *scriptedRunner) reply1(kind exec.CommandKind, out []byte) *scriptedRunner {
	return r.on(kind, func() (exec.SensitiveResult, error) { return stdout(out), nil })
}

func (r *scriptedRunner) RunSensitive(ctx context.Context, cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
	return r.fake.RunSensitive(ctx, cmd)
}

func (r *scriptedRunner) calls() []exec.SensitiveCommand { return r.fake.Calls() }

func newTestAdapter(t *testing.T, run exec.SensitiveRunner, env map[string]string, opts ...Option) *Adapter {
	t.Helper()
	getenv := func(k string) string { return env[k] }
	opts = append([]Option{
		WithLstat(liveSocket(liveInode)),
		WithSelfUID(func() int { return testUID }),
	}, opts...)
	a, err := New(run, testHerdr, getenv, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func newSpec(t *testing.T) (backend.StartSpec, backend.RecoveryTag) {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatalf("NewRecoveryTag: %v", err)
	}
	arg, err := backend.NewBootstrapCommand(exec.Opaque(testBootstrap))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}
	spec, err := backend.NewStartSpec(testCWD, "forgectl", tag, arg)
	if err != nil {
		t.Fatalf("NewStartSpec: %v", err)
	}
	return spec, tag
}

func startClean(t *testing.T) (*Adapter, *scriptedRunner, backend.Ref) {
	t.Helper()
	run := newRunner()
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)
	res := a.Start(context.Background(), spec)
	ref, ok := res.Ref()
	if !ok {
		t.Fatalf("Start did not produce a reference: %v", res.Outcome())
	}
	return a, run, ref
}

func commandOfKind(calls []exec.SensitiveCommand, kind exec.CommandKind) (exec.SensitiveCommand, bool) {
	for _, c := range calls {
		if c.Kind == kind {
			return c, true
		}
	}
	return exec.SensitiveCommand{}, false
}

func causeClass(res backend.StartResult) backend.StartFailureClass {
	cause, ok := res.Cause()
	if !ok {
		return backend.FailureUnspecified
	}
	return cause.Class()
}

// ---------------------------------------------------------------------------
// The pin, and the one command that must not carry it.
// ---------------------------------------------------------------------------

// TestTheSessionRosterIsReadWithoutThePin is a SAFETY property, not a
// stylistic one, and it is the single most important test in this package.
//
// The roster read is deliberately unpinned and every OTHER command is
// deliberately pinned. Both halves are asserted, because either alone is half a
// control.
//
// The REASON is worth stating accurately, since this test originally justified
// itself with a hazard it does not face. `herdr --session <name>` with no
// subcommand — the bare and attach forms — starts a server when that session is
// not running, and a stray herdr server silently capturing later invocations is
// a real hazard in this estate. But herdr's CLI SUBCOMMANDS, which are all this
// adapter issues, return a structured `server_not_running` error rather than
// spawning. Verified by reading herdr's source after two comments claimed
// otherwise in opposite directions.
//
// What the ordering actually buys: the roster is where the socket comes from,
// and refusing a session that is not running gives the operator a better answer
// than `server_not_running` from three commands deeper. The pin still has to be
// on everything else, or a command reaches whatever server the environment
// selects.
func TestTheSessionRosterIsReadWithoutThePin(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)
	ref, ok := res.Ref()
	if !ok {
		t.Fatalf("Start did not produce a reference: %v", res.Outcome())
	}
	a.Probe(context.Background(), ref)

	// The roster read must come FIRST, not merely be unpinned.
	//
	// Asserting only WHICH commands carry the pin left the ordering unpinned:
	// moving the pinned status call above the roster read passed both tests that
	// claim this property. The full suite did go red — but on an unrelated
	// socket-OWNERSHIP test, via its incidental assertion about the call count.
	// Coverage borrowed from a test about something else evaporates the moment
	// that test's shape changes.
	//
	// readiness's first RunSensitive is the roster read on every path, so this
	// holds today and fails for the stated reason if the order ever moves.
	calls := run.calls()
	if len(calls) == 0 {
		t.Fatal("readiness issued no commands at all")
	}
	if !isSessionList(calls[0]) {
		t.Error("the first command was not the session roster read; a pinned command " +
			"issued before the roster has been read reaches a server this adapter never resolved")
	}

	sawRoster := false
	for _, cmd := range calls {
		pinned := len(cmd.Args) >= 2 &&
			cmd.Args[0].Equal(exec.MustFixed("--session")) &&
			cmd.Args[1].Equal(exec.Opaque(defaultSession))
		if isSessionList(cmd) {
			sawRoster = true
			if pinned {
				t.Error("the session roster was read WITH --session; naming a session that " +
					"is not running is what starts one, so this read must never carry the pin")
			}
			continue
		}
		if !pinned {
			t.Errorf("%v does not carry the session pin; it would reach whatever server "+
				"the environment selects", cmd.Kind)
		}
	}
	if !sawRoster {
		t.Error("readiness never read the session roster, so nothing established that " +
			"the session was already running")
	}
}

// TestAStoppedOrAbsentSessionIsRefusedRatherThanStarted pins the refusal that
// keeps the hazard above from firing.
func TestAStoppedOrAbsentSessionIsRefusedRatherThanStarted(t *testing.T) {
	// The SENTINEL is asserted, not just the class. Both refusals are
	// FailureUnavailable, so the class cannot tell them apart — and without the
	// distinction the roster-presence check is untestable, because a name absent
	// from the roster yields a zero row whose Running is false and the second
	// check refuses it anyway.
	cases := map[string]struct {
		roster []byte
		want   error
	}{
		"the session is stopped": {sessionsJSON(map[string]any{
			"default": true, "name": defaultSession, "running": false, "socket_path": testSocket,
		}), ErrSessionNotRunning},
		"no session by that name": {sessionsJSON(map[string]any{
			"default": false, "name": "somebody-elses", "running": true, "socket_path": testSocket,
		}), ErrNoSuchSession},
		"an empty roster": {sessionsJSON(), ErrNoSuchSession},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			roster := tc.roster
			run := newRunner()
			run.sessions = func() (exec.SensitiveResult, error) { return stdout(roster), nil }
			a := newTestAdapter(t, run, nil)
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if res.Outcome() != backend.NotMutated {
				t.Errorf("outcome = %v, want NotMutated", res.Outcome())
			}
			if causeClass(res) != backend.FailureUnavailable {
				t.Errorf("class = %v, want FailureUnavailable", causeClass(res))
			}
			cause, ok := res.Cause()
			if !ok || !errors.Is(cause, tc.want) {
				t.Errorf("cause = %v, want %v — the two refusals send an operator to "+
					"different places and the class cannot carry the difference", cause, tc.want)
			}
			if _, created := commandOfKind(run.calls(), exec.KindHerdrCreate); created {
				t.Error("a create ran against a session that was not established as running")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The create argv and the bootstrap.
// ---------------------------------------------------------------------------

// TestTheCreateArgvCarriesTheMarkerAndTheCWD asserts the WHOLE create argv, for
// the reason the cmux adapter learned the hard way: asserting one argument of
// several leaves the rest free to be deleted or transposed with the suite green.
//
// The label is the one that matters. herdr's create takes no description field,
// so the label is the ONLY thing tying a created workspace back to this launch —
// losing it makes every ambiguous create report NotMutated while a live
// workspace sits orphaned, and makes every Close refuse to clean up a workspace
// forgectl did create.
func TestTheCreateArgvCarriesTheMarkerAndTheCWD(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run, nil)
	spec, tag := newSpec(t)

	a.Start(context.Background(), spec)

	create, ok := commandOfKind(run.calls(), exec.KindHerdrCreate)
	if !ok {
		t.Fatal("Start issued no create command")
	}
	pairs := []struct {
		name string
		flag exec.Arg
		want exec.Arg
	}{
		{"--label", exec.MustFixed("--label"), exec.Opaque(tag.OwnershipName())},
		{"--cwd", exec.MustFixed("--cwd"), spec.CWD()},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			found := false
			for i, arg := range create.Args {
				if !arg.Equal(tc.flag) {
					continue
				}
				found = true
				if i+1 >= len(create.Args) {
					t.Fatalf("%s is the last argument; it carries no value", tc.name)
				}
				if !create.Args[i+1].Equal(tc.want) {
					t.Errorf("%s's value is not the one it should carry", tc.name)
				}
			}
			if !found {
				t.Errorf("the create argv does not carry %s", tc.name)
			}
		})
	}
	// Unfocused: a launch puts a surface somewhere for the harness, and
	// stealing the operator's foreground window is a side effect they did not
	// ask for.
	focused := true
	for _, arg := range create.Args {
		if arg.Equal(exec.MustFixed("--no-focus")) {
			focused = false
		}
	}
	if focused {
		t.Error("the create argv does not pass --no-focus")
	}
}

// TestTheBootstrapIsRunInTheCreatedPane is the assertion whose absence broke the
// tmux adapter, adapted to herdr's two-step shape.
//
// herdr's create takes no command, so the bootstrap is a SECOND call against the
// pane the create just reported. Three things are asserted: that the run targets
// the pane id from the create reply, that the bootstrap is its final operand,
// and that NO end-of-options separator precedes it — see the body for why that
// last one is the opposite of what this comment originally said.
func TestTheBootstrapIsRunInTheCreatedPane(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)

	a.Start(context.Background(), spec)

	var runCmd *exec.SensitiveCommand
	for _, c := range run.calls() {
		if c.Kind != exec.KindHerdrCreate {
			continue
		}
		for _, arg := range c.Args {
			if arg.Equal(exec.MustFixed("run")) {
				cc := c
				runCmd = &cc
			}
		}
	}
	if runCmd == nil {
		t.Fatal("no `pane run` command was issued; the workspace would be an idle shell " +
			"and every launch would die in the handshake")
	}
	last := runCmd.Args[len(runCmd.Args)-1]
	if !last.Equal(spec.Bootstrap().SensitiveArg()) {
		t.Error("the bootstrap is not the final operand of `pane run`")
	}
	// And NO end-of-options separator, which this test originally asserted the
	// presence of — codifying the bug rather than catching it.
	//
	// `pane run` forwards its COMMAND operand to the terminal instead of
	// parsing it, so a `--` is not consumed as a separator: it gets TYPED. A
	// live launch answered `zsh: command not found: --` while the real
	// bootstrap never ran, and the handshake then timed out on a workspace that
	// had been created — the worst shape, because it is a real surface with no
	// harness in it.
	for _, arg := range runCmd.Args {
		if arg.Equal(exec.EndOfOptions()) {
			t.Error("`pane run` carries an end-of-options separator; herdr does not " +
				"consume one here, so it is typed into the pane and the bootstrap never runs")
		}
	}
	targeted := false
	for _, arg := range runCmd.Args {
		if arg.Equal(exec.Opaque(paneA)) {
			targeted = true
		}
	}
	if !targeted {
		t.Error("`pane run` does not target the pane id the create reported")
	}
}

// TestAForeignRootPaneNeverReceivesTheBootstrap is the end-to-end half of the
// child-qualification rule, and it is the half that matters.
//
// The constructor test in backend proves the rule refuses. This proves the rule
// is REACHED BEFORE DELIVERY — because the hazard is not an invalid identity, it
// is the handshake socket path and the one-shot nonce being typed into a pane
// belonging to somebody else's workspace while Start returns a clean RefKnown
// that mentions none of it.
//
// The ordering that makes this work is worth naming: reference() builds the
// identity, and the `pane run` only happens after it succeeds. Move the run
// above the reference and this test goes red while every constructor test stays
// green.
func TestAForeignRootPaneNeverReceivesTheBootstrap(t *testing.T) {
	// Our workspace, somebody else's root pane — a raced reply, or a stale id
	// from a prior incarnation.
	run := newRunner().reply1(exec.KindHerdrCreate, createJSON(wsA, tabA, "wOTHER:p1"))
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	for _, cmd := range run.calls() {
		for _, arg := range cmd.Args {
			if arg.Equal(exec.Opaque("wOTHER:p1")) {
				t.Fatal("a command was addressed to a pane in a workspace this launch " +
					"does not own; the socket path and nonce would go there")
			}
			if arg.Equal(spec.Bootstrap().SensitiveArg()) {
				t.Fatal("the bootstrap was delivered despite the create reply naming a " +
					"foreign root pane")
			}
		}
	}

	// And the outcome is honest rather than a clean success: nothing was found
	// under our marker, so nothing was created that needs closing.
	if res.Outcome() == backend.RefKnown {
		t.Error("a create reply naming a foreign pane produced RefKnown")
	}
	if !res.Failed() {
		t.Error("the launch did not succeed but the result does not say so")
	}
}

// TestACreateThatSucceedsAndAPaneRunThatFailsStillYieldsARef is the contract's
// most awkward case, and herdr is the only backend that has it.
//
// The workspace exists and we know exactly which one it is; the harness did not
// start. Returning a bare failure would STRAND it — the service would have
// nothing to close — so the answer is RefKnown-with-cause: the launch failed AND
// here is what to clean up.
func TestACreateThatSucceedsAndAPaneRunThatFailsStillYieldsARef(t *testing.T) {
	run := newRunner()
	// Driven through RunFunc directly rather than through run.on, because the
	// create KIND covers both the workspace create and the pane run and the
	// scripted path suppresses the second. An earlier version installed a
	// run.on closure here as well and then replaced RunFunc wholesale, so the
	// closure never ran — dead setup that reads as live, and a reader tuning it
	// would have been tuning nothing.
	calls := 0
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrReadiness && isSessionList(cmd) {
			return stdout(defaultSessions()), nil
		}
		switch cmd.Kind {
		case exec.KindHerdrReadiness:
			return stdout(goodStatus()), nil
		case exec.KindHerdrSnapshot:
			return stdout(listJSON()), nil
		case exec.KindHerdrCreate:
			calls++
			if calls == 1 {
				return stdout(createJSON(wsA, tabA, paneA)), nil
			}
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrCreate, exec.OutcomeExit)
		}
		return exec.SensitiveResult{}, nil
	}
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	ref, ok := res.Ref()
	if !ok {
		t.Fatal("a workspace was created and the result carries no reference to close it; " +
			"the surface would be stranded")
	}
	if res.Outcome() != backend.RefKnown {
		t.Errorf("outcome = %v, want RefKnown", res.Outcome())
	}
	if !res.Failed() {
		t.Error("the launch failed but the result does not say so")
	}
	id, err := ref.HerdrIdentity()
	if err != nil {
		t.Fatalf("HerdrIdentity: %v", err)
	}
	if id.Workspace() != wsA {
		t.Errorf("reference names %q, want %q", id.Workspace(), wsA)
	}
}

// ---------------------------------------------------------------------------
// Reading, fail-closed.
// ---------------------------------------------------------------------------

// TestAnUnreadableListingIsNeverAnEmptyOne is the contract both earlier adapters
// had to be taught: a reply we could not read is not a reply saying "nothing
// here". Reporting absence from one is fail-OPEN in the direction that orphans a
// live surface.
func TestAnUnreadableListingIsNeverAnEmptyOne(t *testing.T) {
	unreadable := map[string]exec.SensitiveResult{
		"not JSON at all": stdout([]byte("herdr: something went sideways")),
		"truncated mid-document": {Stdout: exec.BoundedOutputForTest(
			listJSON([2]string{wsA, "x"}), exec.OutputOverflowed)},
		"rows present, none usable": stdout(listJSON([2]string{"", "x"}, [2]string{"w2R:p1", "y"})),
	}
	// A truncated listing carrying OUR OWN marker. Without it the completeness
	// check is masked: every other truncated fixture also fails the ownership
	// read-back, so deleting the truncation guard changes nothing observable and
	// its mutation survives. This is the one shape where the two guards
	// disagree.
	t.Run("close refuses: truncated but carrying our marker", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.on(exec.KindHerdrProbe, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest(
				listJSON([2]string{wsA, ref.OwnershipName()}), exec.OutputOverflowed)}, nil
		})

		got := a.Close(context.Background(), ref)
		if got.State().SatisfiesRollback() {
			t.Error("a listing we could not finish reading was acted on as if complete")
		}
		if _, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup); cleaned {
			t.Error("a close was issued from a truncated listing")
		}
	})
	for name, reply := range unreadable {
		t.Run("close refuses: "+name, func(t *testing.T) {
			a, run, ref := startClean(t)
			run.on(exec.KindHerdrProbe, func() (exec.SensitiveResult, error) { return reply, nil })

			if got := a.Close(context.Background(), ref); got.State().SatisfiesRollback() {
				t.Error("an unreadable listing discharged a rollback obligation still outstanding")
			}
		})
	}
	t.Run("a complete empty listing IS an absence", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON())

		if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
			t.Errorf("Close = %v, want a satisfied rollback", got)
		}
	})
}

// TestOneUnusableRowRefusesTheWholeListing is where this parser deliberately
// diverges from its siblings.
//
// internal/tmux and the cmux adapter drop an unusable row and refuse only when
// EVERY row is unusable, because a tmux session name is chosen by the operator
// and erroring on one malformed row would let anyone who can name a session
// break every lookup. herdr ids are SERVER-assigned, so nothing an operator
// types reaches this field and an unusable row means the server said something
// this adapter does not understand.
//
// The asymmetry is why it matters: a dropped row for OUR workspace reads as
// absence at both consumers — Close discharges a rollback for a live surface,
// and reconcile reports NotMutated for a create that landed. Refusing the
// listing costs a launch that fails loudly instead.
func TestOneUnusableRowRefusesTheWholeListing(t *testing.T) {
	t.Run("a good row beside a bad one is refused", func(t *testing.T) {
		out := exec.BoundedOutputForTest(
			listJSON([2]string{wsA, "fc-surface-x"}, [2]string{"w2R:p1", "y"}), exec.OutputComplete)
		if _, err := parseWorkspaceList(out); err == nil {
			t.Error("a listing carrying an unusable row was accepted, so a row for our " +
				"own workspace could be dropped and read as absence")
		}
	})

	t.Run("close refuses rather than discharging", func(t *testing.T) {
		a, run, ref := startClean(t)
		// Our workspace IS present and correctly marked — but a sibling row is
		// unreadable, so the listing cannot be trusted to be complete.
		run.reply1(exec.KindHerdrProbe,
			listJSON([2]string{wsA, ref.OwnershipName()}, [2]string{"w2R:p1", "y"}))

		if got := a.Close(context.Background(), ref); got.State().SatisfiesRollback() {
			t.Errorf("Close = %v; a listing with an unusable row was acted on", got)
		}
	})

	t.Run("an all-good listing is still accepted", func(t *testing.T) {
		out := exec.BoundedOutputForTest(
			listJSON([2]string{wsA, "fc-surface-x"}, [2]string{wsB, "fc-surface-y"}), exec.OutputComplete)
		rows, err := parseWorkspaceList(out)
		if err != nil {
			t.Fatalf("parseWorkspaceList: %v", err)
		}
		if len(rows) != 2 {
			t.Errorf("rows = %d, want 2", len(rows))
		}
	})
}

// TestACreateReplyMissingAnIDNeverBecomesAReference pins that a create must
// report the FULL identity. A create reports all three ids, so a reply missing
// one is a reply we do not understand rather than a partial success.
func TestACreateReplyMissingAnIDNeverBecomesAReference(t *testing.T) {
	cases := map[string][]byte{
		"no workspace id":      createJSON("", tabA, paneA),
		"no tab id":            createJSON(wsA, "", paneA),
		"no pane id":           createJSON(wsA, tabA, ""),
		"a colon in workspace": createJSON("w2R:t1", tabA, paneA),
		"not JSON":             []byte("OK"),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			run := newRunner().reply1(exec.KindHerdrCreate, reply)
			a := newTestAdapter(t, run, nil)
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if _, ok := res.Ref(); ok {
				t.Fatal("an incomplete create reply produced a reference")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Ownership, incarnation, and the close.
// ---------------------------------------------------------------------------

// TestCloseRefusesAWorkspaceThatIsNotOurs is the ownership read-back the Closer
// contract states as a MUST — and the defect the cmux adapter shipped with,
// caught only by its security review.
//
// A herdr workspace id is server-assigned and carries nothing of ours, so
// Ref.Validate cannot bind it to the tag. A create response naming the wrong
// object yields a fully VALID Ref, and without the read-back Close destroys
// somebody else's workspace and reports success.
func TestCloseRefusesAWorkspaceThatIsNotOurs(t *testing.T) {
	a, run, ref := startClean(t)
	run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, "fc-surface-somebodyelse"}))

	closed := a.Close(context.Background(), ref)
	if closed.State() != backend.CloseIdentityMismatch {
		t.Errorf("Close state = %v, want identity-mismatch", closed.State())
	}
	if _, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup); cleaned {
		t.Fatal("a close was issued against a workspace carrying another owner's marker")
	}
	if probe := a.Probe(context.Background(), ref); probe.State() != backend.ProbeIdentityMismatch {
		t.Errorf("Probe state = %v, want identity-mismatch", probe.State())
	}
}

// TestCloseAcceptsOnlyTheExactMarker is the acceptance side plus the near
// misses, each DERIVED from the reference under test — a fixture built from a
// different tag would be refused for the wrong reason and pass under any
// comparison.
func TestCloseAcceptsOnlyTheExactMarker(t *testing.T) {
	refusals := map[string]func(string) string{
		"empty":              func(string) string { return "" },
		"a prefix of ours":   func(m string) string { return m[:len(m)-1] },
		"ours with a suffix": func(m string) string { return m + "x" },
		"case-folded":        strings.ToUpper,
	}
	for name, plant := range refusals {
		t.Run("refuses "+name, func(t *testing.T) {
			a, run, ref := startClean(t)
			planted := plant(ref.OwnershipName())
			if planted == ref.OwnershipName() {
				t.Fatal("the fixture equals the real marker; this case could not fail")
			}
			run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, planted}))

			if got := a.Close(context.Background(), ref); got.State() != backend.CloseIdentityMismatch {
				t.Errorf("Close state = %v, want identity-mismatch", got.State())
			}
		})
	}
	t.Run("accepts the exact marker", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))

		if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
			t.Fatalf("Close = %v, want a satisfied rollback", got)
		}
		cleanup, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup)
		if !cleaned {
			t.Fatal("no close was issued for a workspace that is ours")
		}
		// The operand is the workspace id and nothing else stands between the
		// verb and it. An end-of-options separator here makes herdr answer
		// `usage: herdr workspace close <workspace_id>` and close NOTHING — a
		// live rollback failed for exactly that reason, reporting "cleanup
		// failed" on a workspace that was still open.
		last := cleanup.Args[len(cleanup.Args)-1]
		if !last.Equal(exec.Opaque(wsA)) {
			t.Error("the close operand is not the reference's workspace id")
		}
		for _, arg := range cleanup.Args {
			if arg.Equal(exec.EndOfOptions()) {
				t.Error("`workspace close` carries an end-of-options separator; herdr " +
					"rejects the command as a usage error and closes nothing")
			}
		}
	})
}

// TestARestartedServerIsAnIdentityMismatchNotAnAbsence pins the incarnation
// check. herdr reports no pid and no start time, so the socket inode is the only
// witness — the weakness forgectl#344 tracks. Without the check, an id the new
// server does not recognise reads as an ordinary absence and Close discharges a
// rollback for a workspace that may still be open on the old incarnation.
func TestARestartedServerIsAnIdentityMismatchNotAnAbsence(t *testing.T) {
	_, _, ref := startClean(t)
	run := newRunner().reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
	restarted := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode+1)))

	if closed := restarted.Close(context.Background(), ref); closed.State().SatisfiesRollback() {
		t.Error("a restarted server discharged a rollback for a workspace it never held")
	}
	if probe := restarted.Probe(context.Background(), ref); probe.State() == backend.ProbePresent {
		t.Error("a restarted server reported the referenced workspace present")
	}
	if _, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup); cleaned {
		t.Error("a close ran against a server the reference was not taken on")
	}
}

// TestAServerRestartingMidCreateNeverBecomesAReference pins the re-fingerprint
// that brackets the mutation.
func TestAServerRestartingMidCreateNeverBecomesAReference(t *testing.T) {
	stats := 0
	before, after := liveSocket(liveInode), liveSocket(liveInode+1)
	seam := func(p string) (os.FileInfo, error) {
		stats++
		if stats == 1 {
			return before(p)
		}
		return after(p)
	}
	run := newRunner()
	a := newTestAdapter(t, run, nil, WithLstat(seam))
	spec, _ := newSpec(t)

	if _, ok := a.Start(context.Background(), spec).Ref(); ok {
		t.Fatal("a reference was minted across a server restart")
	}
}

// TestProbeAnswersItsTwoPositiveOutcomes covers the arms Prober exists for — the
// gap the cmux review found, where every Probe assertion was a refusal and the
// two answering arms had no accepting sibling.
func TestProbeAnswersItsTwoPositiveOutcomes(t *testing.T) {
	t.Run("present when the marked workspace is listed", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))

		if got := a.Probe(context.Background(), ref); got.State() != backend.ProbePresent {
			t.Errorf("Probe state = %v, want present", got.State())
		}
	})
	t.Run("gone on a complete empty listing", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindHerdrProbe, listJSON())

		if got := a.Probe(context.Background(), ref); got.State() != backend.ProbeGone {
			t.Errorf("Probe state = %v, want gone", got.State())
		}
	})
}

// TestAlreadyGoneIsMatchedOnTheStructuredCode is what herdr's error envelope
// buys over cmux's prose. The code is machine-readable and stable; the message
// beside it is explicitly reworded in the fixture to prove nothing reads it.
func TestAlreadyGoneIsMatchedOnTheStructuredCode(t *testing.T) {
	a, run, ref := startClean(t)
	run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
	run.on(exec.KindHerdrCleanup, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
				Stderr: exec.BoundedOutputForTest(errorJSON("workspace_not_found"), exec.OutputComplete),
			},
			exec.SensitiveErrorForTest(exec.KindHerdrCleanup, exec.OutcomeExit)
	})

	if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
		t.Errorf("Close = %v, want a satisfied rollback", got)
	}
}

// TestAFailedCloseIsReportedAsAFailure is the acceptance case beside it: without
// one, a Close that called every failure "already gone" would pass and silently
// discharge every obligation.
func TestAFailedCloseIsReportedAsAFailure(t *testing.T) {
	a, run, ref := startClean(t)
	run.reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
	run.on(exec.KindHerdrCleanup, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
				Stderr: exec.BoundedOutputForTest(errorJSON("internal_error"), exec.OutputComplete),
			},
			exec.SensitiveErrorForTest(exec.KindHerdrCleanup, exec.OutcomeExit)
	})

	if got := a.Close(context.Background(), ref); got.State().SatisfiesRollback() {
		t.Errorf("Close = %v, want an unsatisfied rollback", got)
	}
}

// ---------------------------------------------------------------------------
// Readiness refusals.
// ---------------------------------------------------------------------------

// TestReadinessRefusesASocketItCannotProveIsOurs covers all the ownership arms
// under one criterion, because they are one control: the bootstrap is only safe
// to send to a socket this uid owns.
func TestReadinessRefusesASocketItCannotProveIsOurs(t *testing.T) {
	cases := map[string]fakeInfo{
		"owned by another user": {sys: &syscall.Stat_t{Ino: liveInode, Uid: testUID + 1}, mode: fs.ModeSocket | 0o600},
		"owner cannot be read":  {sys: nil, mode: fs.ModeSocket | 0o600},
		"not a socket at all":   {sys: &syscall.Stat_t{Ino: liveInode, Uid: testUID}, mode: 0o600},
	}
	for name, info := range cases {
		t.Run(name, func(t *testing.T) {
			run := newRunner()
			a := newTestAdapter(t, run, nil,
				WithLstat(func(string) (os.FileInfo, error) { return info, nil }))
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if res.Outcome() != backend.NotMutated {
				t.Errorf("outcome = %v, want NotMutated", res.Outcome())
			}
			if _, created := commandOfKind(run.calls(), exec.KindHerdrCreate); created {
				t.Error("a create ran against a socket whose ownership could not be established")
			}
		})
	}
}

// TestAnUnreadableOwnerRefusesEvenWhenTheUIDsWouldHaveMatched isolates the
// cannot-establish arm from the comparison beside it. Under any non-root uid the
// zero owner an unreadable stat yields differs from the real uid anyway, so only
// uid 0 separates them — and that is the account where it matters most.
func TestAnUnreadableOwnerRefusesEvenWhenTheUIDsWouldHaveMatched(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run, nil,
		WithLstat(func(string) (os.FileInfo, error) {
			return fakeInfo{sys: nil, mode: fs.ModeSocket | 0o600}, nil
		}),
		WithSelfUID(func() int { return 0 }))
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if causeClass(res) != backend.FailurePermissionDenied {
		t.Errorf("class = %v, want FailurePermissionDenied", causeClass(res))
	}
	if len(run.calls()) > 1 {
		t.Error("a command beyond the roster read reached a socket whose owner is unknown")
	}
}

// TestReadinessRefusesAnIncompatibleServer covers herdr's own compatibility
// verdict and ours. Both are checked because they are different claims:
// `compatible` can be true across a protocol this adapter has never seen.
func TestReadinessRefusesAnIncompatibleServer(t *testing.T) {
	cases := map[string][]byte{
		"herdr says the pair is incompatible": statusJSON(true, false, minProtocol),
		"the protocol predates our floor":     statusJSON(true, true, minProtocol-1),
		"the session stopped between calls":   statusJSON(false, true, minProtocol),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			run := newRunner().reply1(exec.KindHerdrReadiness, reply)
			a := newTestAdapter(t, run, nil)
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if res.Outcome() != backend.NotMutated {
				t.Errorf("outcome = %v, want NotMutated", res.Outcome())
			}
			if _, created := commandOfKind(run.calls(), exec.KindHerdrCreate); created {
				t.Error("a create ran against a server we could not agree with")
			}
		})
	}
}

// TestReadinessRefusesASocketPathHerdrShouldNotHaveReported pins that the
// endpoint comes from herdr and is therefore checked rather than trusted.
func TestReadinessRefusesASocketPathHerdrShouldNotHaveReported(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"relative":      "herdr.sock",
		"unclean":       "/tmp/../tmp/herdr.sock",
		"past sun_path": "/tmp/" + strings.Repeat("a", maxSocketPathLen) + "/herdr.sock",
	}
	for name, socket := range cases {
		t.Run(name, func(t *testing.T) {
			run := newRunner()
			run.sessions = func() (exec.SensitiveResult, error) {
				return stdout(sessionsJSON(map[string]any{
					"default": true, "name": defaultSession, "running": true, "socket_path": socket,
				})), nil
			}
			a := newTestAdapter(t, run, nil)
			spec, _ := newSpec(t)

			if res := a.Start(context.Background(), spec); res.Outcome() != backend.NotMutated {
				t.Errorf("outcome = %v, want NotMutated", res.Outcome())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Reconciliation.
// ---------------------------------------------------------------------------

// TestReconciliationMatchesOnTheMarkerAndOnNovelty pins both halves of the match
// condition, each case removing exactly one.
func TestReconciliationMatchesOnTheMarkerAndOnNovelty(t *testing.T) {
	tests := map[string]struct {
		before, after func(marker string) []byte
		want          backend.MutationOutcome
	}{
		"exactly one new marked workspace": {
			before: func(string) []byte { return listJSON() },
			after:  func(m string) []byte { return listJSON([2]string{wsA, m}) },
			want:   backend.RefKnown,
		},
		"a marked workspace that predates this attempt": {
			before: func(m string) []byte { return listJSON([2]string{wsA, m}) },
			after:  func(m string) []byte { return listJSON([2]string{wsA, m}) },
			want:   backend.NotMutated,
		},
		"a new workspace wearing somebody else's marker": {
			before: func(string) []byte { return listJSON() },
			after:  func(string) []byte { return listJSON([2]string{wsA, "fc-surface-notours"}) },
			want:   backend.NotMutated,
		},
		"two new workspaces wearing our marker": {
			before: func(string) []byte { return listJSON() },
			after: func(m string) []byte {
				return listJSON([2]string{wsA, m}, [2]string{wsB, m})
			},
			want: backend.OutcomeUnknown,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec, tag := newSpec(t)
			marker := tag.OwnershipName()
			run := newRunner().
				reply1(exec.KindHerdrSnapshot, tc.before(marker)).
				reply1(exec.KindHerdrCreate, []byte("not a create reply")).
				reply1(exec.KindHerdrReconcile, tc.after(marker))
			a := newTestAdapter(t, run, nil)

			if res := a.Start(context.Background(), spec); res.Outcome() != tc.want {
				t.Errorf("outcome = %v, want %v", res.Outcome(), tc.want)
			}
		})
	}
}

// TestAFailedPreSnapshotStopsTheLaunch pins the check that lets reconciliation
// tell this attempt's workspace from a retry's leftover.
func TestAFailedPreSnapshotStopsTheLaunch(t *testing.T) {
	run := newRunner().on(exec.KindHerdrSnapshot, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrSnapshot, exec.OutcomeExit)
	})
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("outcome = %v, want NotMutated", res.Outcome())
	}
	if _, created := commandOfKind(run.calls(), exec.KindHerdrCreate); created {
		t.Error("a create ran without a pre-snapshot to reconcile against")
	}
}

// ---------------------------------------------------------------------------
// Session resolution.
// ---------------------------------------------------------------------------

// TestResolveSessionRecordsTheChainThatChoseIt pins that the SOURCE differs by
// chain, which is what makes a reference portable: one taken against a named
// session must not be answered by an adapter that resolved the default.
func TestResolveSessionRecordsTheChainThatChoseIt(t *testing.T) {
	tests := map[string]struct {
		env     map[string]string
		want    backend.ServerSource
		session string
	}{
		"the default session": {
			env: nil, want: backend.HerdrDefaultSessionServer(), session: defaultSession,
		},
		"a named session": {
			env:  map[string]string{sessionEnv: "fleet"},
			want: backend.HerdrNamedSessionServer(), session: "fleet",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			session, source, err := resolveSession(func(k string) string { return tc.env[k] })
			if err != nil {
				t.Fatalf("resolveSession: %v", err)
			}
			if session != tc.session {
				t.Errorf("session = %q, want %q", session, tc.session)
			}
			if source != tc.want {
				t.Errorf("source = %v, want %v", source, tc.want)
			}
		})
	}
}

// TestResolveSessionRefusesANameThatIsNotOneOperand keeps the pin to a shape
// that cannot be read as another argument. The name reaches a command line, so a
// leading dash or embedded whitespace is either not a session name or is trying
// to be a flag.
func TestResolveSessionRefusesANameThatIsNotOneOperand(t *testing.T) {
	bad := []string{
		"-fleet", "--session", "fleet name", "fleet\tname", "fleet\nname",
		"fleet;rm", "fleet/../other", strings.Repeat("f", maxSessionNameLen+1), "fleet\x1b[2J",
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			_, _, err := resolveSession(func(string) string { return name })
			if !errors.Is(err, ErrResolveSession) {
				t.Errorf("resolveSession(%q) = %v, want ErrResolveSession", name, err)
			}
		})
	}
	for _, name := range []string{"fleet", "my-session", "my_session", "v1.2", "default"} {
		t.Run("accepts "+name, func(t *testing.T) {
			got, _, err := resolveSession(func(string) string { return name })
			if err != nil {
				t.Errorf("resolveSession(%q) = %v, want acceptance", name, err)
			}
			if got != name {
				t.Errorf("session = %q, want %q unchanged", got, name)
			}
		})
	}
}

// TestNewRefusesARelativeBinaryPath pins why the CLI does the PATH lookup: the
// sensitive runner will not let exec.LookPath choose the binary against the live
// process PATH.
func TestNewRefusesARelativeBinaryPath(t *testing.T) {
	getenv := func(string) string { return "" }
	if _, err := New(newRunner(), "herdr", getenv); !errors.Is(err, ErrResolveSession) {
		t.Errorf("New with a relative path = %v, want ErrResolveSession", err)
	}
	if _, err := New(nil, testHerdr, getenv); !errors.Is(err, ErrResolveSession) {
		t.Errorf("New with no runner = %v, want ErrResolveSession", err)
	}
}

// TestKindReportsHerdr is small but not trivial: surfaceAdapterFor routes on it,
// and an adapter reporting the wrong kind would be handed another backend's
// references.
func TestKindReportsHerdr(t *testing.T) {
	if got := newTestAdapter(t, newRunner(), nil).Kind(); got != backend.KindHerdr {
		t.Errorf("Kind() = %v, want herdr", got)
	}
}

// TestCloseAndProbeRefuseAReferenceForAnotherBackend pins what the kind check
// BUYS — the outcome state, not a prevented close.
func TestCloseAndProbeRefuseAReferenceForAnotherBackend(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run, nil)
	foreign := foreignRef(t)

	closed := a.Close(context.Background(), foreign)
	if closed.State() != backend.CloseUnreadable {
		t.Errorf("Close state = %v, want unreadable", closed.State())
	}
	cause, ok := closed.Cause()
	if !ok || !errors.Is(cause, backend.ErrRefKindMismatch) {
		t.Errorf("Close cause = %v, want ErrRefKindMismatch", cause)
	}
	if probe := a.Probe(context.Background(), foreign); probe.State() != backend.ProbeUnreadable {
		t.Errorf("Probe state = %v, want unreadable", probe.State())
	}
	if len(run.calls()) != 0 {
		t.Error("a command reached herdr for a reference that is not a herdr reference")
	}
}

// TestTheZeroLocateStateNeverClosesAndNeverReportsPresent pins the enum, which
// is the tested half of guards whose default arms cannot be driven.
func TestTheZeroLocateStateNeverClosesAndNeverReportsPresent(t *testing.T) {
	var zero locateState
	if zero == locateFound {
		t.Fatal("the zero locateState is locateFound; every unknown-state guard sits " +
			"on the wrong side of the value it names")
	}
	if zero != locateInvalid {
		t.Errorf("the zero locateState is %d, want locateInvalid", zero)
	}
}

// TestClassifyRunErrorReadsTheSeamsSentinels pins that classification runs on
// the SEAM's vocabulary — *SensitiveError unwraps only to its outcome's package
// sentinel, so a branch written against context.Canceled is unreachable.
func TestClassifyRunErrorReadsTheSeamsSentinels(t *testing.T) {
	cases := map[exec.Outcome]backend.StartFailureClass{
		exec.OutcomeCanceled: backend.FailureCanceled,
		exec.OutcomeTimeout:  backend.FailureTimeout,
		exec.OutcomeInvalid:  backend.FailureInternal,
		exec.OutcomeExit:     backend.FailureUnavailable,
	}
	a := newTestAdapter(t, newRunner(), nil)
	for outcome, want := range cases {
		got := a.classifyRunError(exec.SensitiveErrorForTest(exec.KindHerdrProbe, outcome), exec.SensitiveResult{})
		if got.Class() != want {
			t.Errorf("outcome %v classified as %v, want %v", outcome, got.Class(), want)
		}
	}
}

// TestHerdrErrorCodesGetTheirOwnClasses pins the structured-code arms, which are
// what herdr's envelope buys over prose matching.
func TestHerdrErrorCodesGetTheirOwnClasses(t *testing.T) {
	cases := map[string]backend.StartFailureClass{
		"workspace_not_found": backend.FailureIdentityMismatch,
		"permission_denied":   backend.FailurePermissionDenied,
		"protocol_mismatch":   backend.FailureIncompatible,
		"some_new_code":       backend.FailureUnavailable,
	}
	a := newTestAdapter(t, newRunner(), nil)
	for code, want := range cases {
		res := exec.SensitiveResult{
			Stderr: exec.BoundedOutputForTest(errorJSON(code), exec.OutputComplete),
		}
		got := a.classifyRunError(exec.SensitiveErrorForTest(exec.KindHerdrProbe, exec.OutcomeExit), res)
		if got.Class() != want {
			t.Errorf("code %q classified as %v, want %v", code, got.Class(), want)
		}
	}
}

// TestAHerdrReferenceFromAnotherSelectionChainIsAMismatch drives the source
// check on its own.
//
// It needs a reference that is genuinely herdr — so the kind check passes and
// the identity accessor answers — but was taken through the OTHER session
// chain. That is a real scenario: a reference taken while HERDR_SESSION named a
// session must not be answered by an adapter that resolved the default one,
// even though both are herdr.
func TestAHerdrReferenceFromAnotherSelectionChainIsAMismatch(t *testing.T) {
	_, _, ref := startClean(t) // taken through the default chain
	id, err := ref.HerdrIdentity()
	if err != nil {
		t.Fatalf("HerdrIdentity: %v", err)
	}
	other, err := backend.NewHerdrRef(backend.HerdrNamedSessionServer(), ref.Server(), ref.Tag(), id)
	if err != nil {
		t.Fatalf("NewHerdrRef: %v", err)
	}

	run := newRunner()
	a := newTestAdapter(t, run, nil) // still the default chain

	if closed := a.Close(context.Background(), other); closed.State() != backend.CloseIdentityMismatch {
		t.Errorf("Close state = %v, want identity-mismatch", closed.State())
	}
	if probe := a.Probe(context.Background(), other); probe.State() != backend.ProbeIdentityMismatch {
		t.Errorf("Probe state = %v, want identity-mismatch", probe.State())
	}
	if len(run.calls()) != 0 {
		t.Error("a command reached herdr for a reference from a different selection chain")
	}
}

// TestAVanishedEndpointIsGoneNotUnreadable is the control the code review found
// DEAD, and whose comment had claimed it worked.
//
// locate concludes absence from the failure class AND a stat of the endpoint.
// Every failing readiness path used to return a zero serverInfo, and serverGone
// short-circuits on an empty socket — so the stat never ran and locateAbsent was
// unreachable. A stopped session produced CloseUnreadable forever: a rollback
// obligation for a surface that provably no longer exists, which could never be
// discharged, and a Probe that answered unreadable rather than gone.
//
// Both directions are covered, because the class gate is what stops the fix from
// becoming the opposite defect — a refused socket read as an absent one.
func TestAVanishedEndpointIsGoneNotUnreadable(t *testing.T) {
	t.Run("the socket is gone: already-gone, and the rollback discharges", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner()
		a := newTestAdapter(t, run, nil, WithLstat(absentSocket))

		closed := a.Close(context.Background(), ref)
		if closed.State() != backend.CloseAlreadyGone {
			t.Errorf("Close state = %v, want already-gone", closed.State())
		}
		if !closed.State().SatisfiesRollback() {
			t.Error("a surface that provably no longer exists left its rollback outstanding")
		}
		if probe := a.Probe(context.Background(), ref); probe.State() != backend.ProbeGone {
			t.Errorf("Probe state = %v, want gone", probe.State())
		}
	})

	t.Run("the socket is refused, not absent: unreadable", func(t *testing.T) {
		_, _, ref := startClean(t)
		// Readable and stat-able, but owned by somebody else —
		// FailurePermissionDenied, which the class gate must keep OUT of the
		// absence branch. Without it, carrying the socket forward would turn
		// every refusal into "gone".
		run := newRunner()
		a := newTestAdapter(t, run, nil, WithLstat(func(string) (os.FileInfo, error) {
			return fakeInfo{sys: &syscall.Stat_t{Ino: liveInode, Uid: testUID + 1}, mode: fs.ModeSocket | 0o600}, nil
		}))

		closed := a.Close(context.Background(), ref)
		if closed.State().SatisfiesRollback() {
			t.Errorf("Close = %v; a socket we are REFUSED was read as one that is GONE", closed)
		}
		if probe := a.Probe(context.Background(), ref); probe.State() != backend.ProbeUnreadable {
			t.Errorf("Probe state = %v, want unreadable", probe.State())
		}
	})

	t.Run("a close that fails while the endpoint vanished still discharges", func(t *testing.T) {
		_, _, ref := startClean(t)
		// Present through locate, absent by the time the failed close asks — a
		// herdr that quit mid-rollback. cmux has this arm; herdr had only the
		// structured-code half, so this yielded CloseFailed for a workspace that
		// could not possibly still exist.
		stats := 0
		live := liveSocket(liveInode)
		seam := func(path string) (os.FileInfo, error) {
			stats++
			if stats > 1 {
				return absentSocket(path)
			}
			return live(path)
		}
		run := newRunner().reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.on(exec.KindHerdrCleanup, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrCleanup, exec.OutcomeExit)
		})
		a := newTestAdapter(t, run, nil, WithLstat(seam))

		if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
			t.Errorf("Close = %v, want a satisfied rollback against an endpoint that is gone", got)
		}
	})
}

// TestTheAbsenceGateNeedsBothHalves drives the two conditions SEPARATELY, which
// the sibling test above cannot.
//
// Absence needs a FailureUnavailable class AND an absent socket. The obvious
// fixtures never discriminate: when the socket is simply missing, readiness
// fails Unavailable at its own stat, so both halves agree and either alone
// suffices. Each half only shows its work where the two DISAGREE.
//
// That is a race in both cases, and a real one — readiness stats the socket and
// the absence check re-stats it, so anything can happen in between.
func TestTheAbsenceGateNeedsBothHalves(t *testing.T) {
	// Live for readiness's ownership stat, gone by the time the absence check
	// asks. Everything after the first stat reports ENOENT.
	vanishing := func() func(string) (os.FileInfo, error) {
		stats := 0
		live := liveSocket(liveInode)
		return func(path string) (os.FileInfo, error) {
			stats++
			if stats > 1 {
				return absentSocket(path)
			}
			return live(path)
		}
	}

	t.Run("wrong class, absent socket: NOT gone", func(t *testing.T) {
		// An INCOMPATIBLE server, with the socket vanishing underneath. The
		// stat alone would say "gone"; the class says we never established
		// anything about the workspace. Without the class gate, an
		// incompatible — or unauthenticated — server whose socket happened to
		// vanish would discharge a rollback on a live surface.
		_, _, ref := startClean(t)
		run := newRunner().reply1(exec.KindHerdrReadiness, statusJSON(true, false, minProtocol))
		a := newTestAdapter(t, run, nil, WithLstat(vanishing()))

		// Close only. The seam is stateful — every stat after the first reports
		// ENOENT — so by a second call the socket is genuinely absent from the
		// very first stat, readiness fails Unavailable at its own lstat, and
		// "gone" becomes the correct answer. Asserting Probe here would be
		// asserting against a different world than the one Close saw, which is
		// its own species of test that cannot mean what it says.
		closed := a.Close(context.Background(), ref)
		if closed.State().SatisfiesRollback() {
			t.Errorf("Close = %v; an incompatible server discharged the rollback because "+
				"its socket vanished between two stats", closed)
		}
	})

	t.Run("right class, present socket: NOT gone", func(t *testing.T) {
		// The session reports itself stopped AFTER the roster said it was
		// running — FailureUnavailable — while the socket file is still there.
		// The class alone would say "gone"; the stat says we cannot prove it.
		_, _, ref := startClean(t)
		run := newRunner().reply1(exec.KindHerdrReadiness, statusJSON(false, true, minProtocol))
		a := newTestAdapter(t, run, nil) // socket present throughout

		closed := a.Close(context.Background(), ref)
		if closed.State().SatisfiesRollback() {
			t.Errorf("Close = %v; absence was concluded from the failure class alone, "+
				"with the endpoint still on disk", closed)
		}
		if probe := a.Probe(context.Background(), ref); probe.State() == backend.ProbeGone {
			t.Error("Probe reported gone with the endpoint still present")
		}
	})
}

// TestAFailedReconciliationChoosesByFailureClass covers the branch deciding
// whether a create attempt left something behind — the only path that can report
// NotMutated AFTER a create has run, so a wrong answer is the
// orphan-a-live-surface hazard exactly.
//
// Both outcomes were unasserted: opposite mutants, always-fire and never-fire,
// both survived, which means nothing drove a failing reconciliation at all.
func TestAFailedReconciliationChoosesByFailureClass(t *testing.T) {
	failListing := func(r *scriptedRunner) {
		r.on(exec.KindHerdrReconcile, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrReconcile, exec.OutcomeExit)
		})
	}

	t.Run("the endpoint is still there: outcome-unknown", func(t *testing.T) {
		run := newRunner().reply1(exec.KindHerdrCreate, []byte("not a create reply"))
		failListing(run)
		a := newTestAdapter(t, run, nil)
		spec, _ := newSpec(t)

		if res := a.Start(context.Background(), spec); res.Outcome() != backend.OutcomeUnknown {
			t.Errorf("outcome = %v, want OutcomeUnknown — a listing we could not read "+
				"is not proof nothing was created", res.Outcome())
		}
	})

	t.Run("the endpoint has vanished: not-mutated", func(t *testing.T) {
		stats := 0
		live := liveSocket(liveInode)
		seam := func(path string) (os.FileInfo, error) {
			stats++
			if stats > 1 {
				return absentSocket(path)
			}
			return live(path)
		}
		run := newRunner().reply1(exec.KindHerdrCreate, []byte("not a create reply"))
		failListing(run)
		a := newTestAdapter(t, run, nil, WithLstat(seam))
		spec, _ := newSpec(t)

		if res := a.Start(context.Background(), spec); res.Outcome() != backend.NotMutated {
			t.Errorf("outcome = %v, want NotMutated", res.Outcome())
		}
	})
}

// TestThePaneRunFailureCarriesHerdrsErrorCode pins that the failing bootstrap
// call's RESULT reaches classification.
//
// It was discarded, so the structured-code switch read an empty stderr and every
// pane-run failure classified as FailureUnavailable — on the one call where
// herdr's code is most worth having. A permission problem became "check whether
// herdr is up".
func TestThePaneRunFailureCarriesHerdrsErrorCode(t *testing.T) {
	calls := 0
	run := newRunner()
	run.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if cmd.Kind == exec.KindHerdrReadiness && isSessionList(cmd) {
			return stdout(defaultSessions()), nil
		}
		switch cmd.Kind {
		case exec.KindHerdrReadiness:
			return stdout(goodStatus()), nil
		case exec.KindHerdrSnapshot:
			return stdout(listJSON()), nil
		case exec.KindHerdrCreate:
			calls++
			if calls == 1 {
				return stdout(createJSON(wsA, tabA, paneA)), nil
			}
			return exec.SensitiveResult{
					Stderr: exec.BoundedOutputForTest(errorJSON("permission_denied"), exec.OutputComplete),
				},
				exec.SensitiveErrorForTest(exec.KindHerdrCreate, exec.OutcomeExit)
		}
		return exec.SensitiveResult{}, nil
	}
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if _, ok := res.Ref(); !ok {
		t.Fatal("the workspace was created and the result carries no reference to close it")
	}
	if causeClass(res) != backend.FailurePermissionDenied {
		t.Errorf("class = %v, want FailurePermissionDenied — the failing pane run's "+
			"stderr never reached classification", causeClass(res))
	}
}

// TestParseSessionsFailsClosed covers the roster parser's two guards, whose
// siblings in parseWorkspaceList are tested. One criterion, both parsers.
func TestParseSessionsFailsClosed(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		out := exec.BoundedOutputForTest(defaultSessions(), exec.OutputOverflowed)
		if _, err := parseSessions(out); err == nil {
			t.Error("a roster we could not finish reading was accepted")
		}
	})
	t.Run("rows present, none usable", func(t *testing.T) {
		out := exec.BoundedOutputForTest(sessionsJSON(
			map[string]any{"name": "", "running": true, "socket_path": testSocket},
		), exec.OutputComplete)
		if _, err := parseSessions(out); err == nil {
			t.Error("a roster whose every row was unusable was reported as empty")
		}
	})
	t.Run("a complete empty roster is a real answer", func(t *testing.T) {
		rows, err := parseSessions(exec.BoundedOutputForTest(sessionsJSON(), exec.OutputComplete))
		if err != nil {
			t.Fatalf("parseSessions: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("rows = %d, want 0", len(rows))
		}
	})
}

// foreignRef builds a valid reference belonging to a different backend.
func foreignRef(t *testing.T) backend.Ref {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatalf("NewRecoveryTag: %v", err)
	}
	server, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/tmp/tmux-501/default", Version: "tmux 3.7b", Inode: liveInode,
	})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	id, err := backend.NewTmuxIdentity(tag.OwnershipName())
	if err != nil {
		t.Fatalf("NewTmuxIdentity: %v", err)
	}
	ref, err := backend.NewTmuxRef(backend.TmuxDefaultServer(), server, tag, id)
	if err != nil {
		t.Fatalf("NewTmuxRef: %v", err)
	}
	return ref
}
