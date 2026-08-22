package cmuxadapter

import (
	"bytes"
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
	testSocket    = "/tmp/cmuxtest/cmux.sock"
	testCmux      = "/opt/homebrew/bin/cmux"
	testCWD       = "/Users/example/code/forgectl"
	testBootstrap = "forgectl surface _exec --protocol 1"
	testUID       = 501
	liveInode     = 4242
	// workspaceA and workspaceB are the two UUIDs the fixtures use. Written out
	// rather than generated so a failure names a constant a reader can find.
	workspaceA = "39D03BE6-9444-4A2B-9C24-ABA8C1126A0A"
	workspaceB = "E08217A6-0274-48B0-BA48-CD8E8BEC8817"
)

// fakeInfo is the minimum os.FileInfo the fingerprint and ownership paths read.
// Sys() returns nil rather than a *syscall.Stat_t when a test needs the platform
// stat to be unavailable — which is how the fail-closed arms are driven.
type fakeInfo struct {
	sys  any
	mode fs.FileMode
}

func (fakeInfo) Name() string        { return "cmux.sock" }
func (fakeInfo) Size() int64         { return 0 }
func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (fakeInfo) ModTime() time.Time  { return time.Unix(0, 0) }
func (f fakeInfo) IsDir() bool       { return f.mode.IsDir() }
func (f fakeInfo) Sys() any          { return f.sys }

// liveSocket is an lstat seam reporting a socket owned by this uid with a usable
// inode, so both the ownership check and Fingerprint's non-zero-inode
// requirement are met.
func liveSocket(inode uint64) func(string) (os.FileInfo, error) {
	return func(string) (os.FileInfo, error) {
		return fakeInfo{
			sys:  &syscall.Stat_t{Ino: inode, Uid: testUID},
			mode: fs.ModeSocket | 0o600,
		}, nil
	}
}

// absentSocket is the clean-machine seam: nothing is listening.
func absentSocket(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

// capabilitiesJSON renders a readiness reply. Every field a test wants to vary
// is a parameter, so no test asserts against a fixture that differs from the
// production shape in more ways than the one it names.
func capabilitiesJSON(protocol string, version int, socket string, methods []string) []byte {
	doc := map[string]any{
		"protocol":    protocol,
		"version":     version,
		"socket_path": socket,
		"access_mode": "password",
		"methods":     methods,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}

func goodCapabilities() []byte {
	return capabilitiesJSON(wantProtocol, minVersion, testSocket, requiredMethods)
}

// listJSON renders a workspace listing from id→description pairs.
func listJSON(rows ...[2]string) []byte {
	workspaces := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		workspaces = append(workspaces, map[string]any{
			"id":           r[0],
			"description":  r[1],
			"custom_title": "forgectl",
		})
	}
	raw, err := json.Marshal(map[string]any{"window_id": "W", "workspaces": workspaces})
	if err != nil {
		panic(err)
	}
	return raw
}

// createJSON renders a create reply under --id-format uuids.
func createJSON(id string) []byte {
	raw, err := json.Marshal(map[string]any{
		"workspace_id": id,
		"surface_id":   "S",
		"window_id":    "W",
		"group_id":     nil,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func stdout(b []byte) exec.SensitiveResult {
	return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest(b, exec.OutputComplete)}
}

// scriptedRunner answers each CommandKind from a table, so a test names only the
// replies it cares about and every other verb still behaves.
type scriptedRunner struct {
	fake  exec.FakeSensitiveRunner
	reply map[exec.CommandKind]func() (exec.SensitiveResult, error)
}

func newRunner() *scriptedRunner {
	r := &scriptedRunner{reply: map[exec.CommandKind]func() (exec.SensitiveResult, error){}}
	r.fake.RunFunc = func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		if fn, ok := r.reply[cmd.Kind]; ok {
			return fn()
		}
		switch cmd.Kind {
		case exec.KindCmuxReadiness:
			return stdout(goodCapabilities()), nil
		case exec.KindCmuxSnapshot, exec.KindCmuxReconcile, exec.KindCmuxProbe:
			return stdout(listJSON()), nil
		case exec.KindCmuxCreate:
			return stdout(createJSON(workspaceA)), nil
		default:
			return exec.SensitiveResult{}, nil
		}
	}
	return r
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

func newTestAdapter(t *testing.T, run exec.SensitiveRunner, opts ...Option) *Adapter {
	t.Helper()
	env := map[string]string{"CMUX_SOCKET_PATH": testSocket}
	getenv := func(k string) string { return env[k] }
	// The live socket goes first so every test is independent of the real
	// filesystem on the machine running it, and a caller's own WithLstat still
	// wins, options being applied in order.
	opts = append([]Option{
		WithLstat(liveSocket(liveInode)),
		WithSelfUID(func() int { return testUID }),
	}, opts...)
	a, err := New(run, testCmux, getenv, opts...)
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

// startClean drives one successful Start and returns the reference it produced,
// so a test needing a Ref for Close or Probe is not hand-building a shape
// Ref.Validate might not accept.
func startClean(t *testing.T) (*Adapter, *scriptedRunner, backend.Ref) {
	t.Helper()
	run := newRunner()
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)
	res := a.Start(context.Background(), spec)
	ref, ok := res.Ref()
	if !ok {
		t.Fatalf("Start did not produce a reference: %v", res.Outcome())
	}
	return a, run, ref
}

// ---------------------------------------------------------------------------
// The bootstrap, and the reason this test exists at all.
// ---------------------------------------------------------------------------

// TestStartSendsTheBootstrapAsTheWorkspacesCommand is the single most important
// test in this package, and it exists because the tmux adapter shipped without
// its equivalent: the sealed bootstrap never reached the create argv, so every
// launch made an idle shell, timed out in the handshake, and rolled back — while
// Start reported RefKnown the whole way.
//
// It asserts by rebuilding the expected argument with the same constructor and
// comparing with Equal, never by reading the value back out: the payload has no
// exported accessor, deliberately, because it carries the handshake socket path
// and the one-shot nonce.
func TestStartSendsTheBootstrapAsTheWorkspacesCommand(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	a.Start(context.Background(), spec)

	create, ok := commandOfKind(run.calls(), exec.KindCmuxCreate)
	if !ok {
		t.Fatal("Start issued no create command")
	}
	want := spec.Bootstrap().SensitiveArg()
	found := false
	for i, arg := range create.Args {
		if !arg.Equal(want) {
			continue
		}
		found = true
		// Position matters as much as presence: cmux takes the bootstrap as the
		// VALUE of --command, so an argument that landed anywhere else would be
		// a different flag's operand or a stray positional.
		if i == 0 || !create.Args[i-1].Equal(exec.MustFixed("--command")) {
			t.Error("the bootstrap is in the argv but is not --command's value")
		}
	}
	if !found {
		t.Error("the create argv does not carry the bootstrap; every launch would " +
			"create an idle workspace and die in the handshake")
	}
}

// TestTheCreateArgvCarriesEveryFlagWithItsOwnValue asserts the WHOLE create
// argv, and it exists because asserting one argument of five was the gap.
//
// The bootstrap test below covers `--command` and nothing else covered the
// rest, so deleting `--description`, `--cwd`, or `--name` — or swapping any of
// their values for another sealed argument — left the entire suite green.
// Measured: six such mutants all survived.
//
// The ownership marker is the one that matters most, because it is the only
// thing tying a created workspace back to this launch, and losing it fires both
// hazard directions at once. Reconciliation would match zero rows, so every
// ambiguous create reports NotMutated while a live workspace sits orphaned with
// the harness inside it. And locate would find `row.marker == ""` against our
// name, so every Close returns identity-mismatch and rollback refuses to clean
// up a workspace forgectl really did create.
//
// `--cwd` is a live hazard on its own: cmux exits ZERO when the directory does
// not exist, so a swapped value opens the surface somewhere else and nothing
// notices (forgectl#362).
//
// Values are compared by rebuilding them with the same constructor — the sealed
// ones have no accessor — and each is required to FOLLOW ITS OWN FLAG, because
// presence alone cannot tell a correct argv from one whose values were
// transposed.
func TestTheCreateArgvCarriesEveryFlagWithItsOwnValue(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run)
	spec, tag := newSpec(t)

	a.Start(context.Background(), spec)

	create, ok := commandOfKind(run.calls(), exec.KindCmuxCreate)
	if !ok {
		t.Fatal("Start issued no create command")
	}

	// The flag args are built with literals because exec.MustFixed takes a
	// constant-only type — the seam enforcing "fixed arguments are literals" in
	// the type system, so a runtime string cannot become one even in a test.
	pairs := []struct {
		name string
		flag exec.Arg
		want exec.Arg
	}{
		{"--focus", exec.MustFixed("--focus"), exec.MustFixed("false")},
		{"--description", exec.MustFixed("--description"), exec.Opaque(tag.OwnershipName())},
		{"--name", exec.MustFixed("--name"), spec.Name()},
		{"--cwd", exec.MustFixed("--cwd"), spec.CWD()},
		{"--command", exec.MustFixed("--command"), spec.Bootstrap().SensitiveArg()},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			flag, want := tc.flag, tc.want
			found := false
			for i, arg := range create.Args {
				if !arg.Equal(flag) {
					continue
				}
				found = true
				if i+1 >= len(create.Args) {
					t.Fatalf("%s is the last argument; it carries no value", tc.name)
				}
				if !create.Args[i+1].Equal(want) {
					t.Errorf("%s's value is not the one it should carry", tc.name)
				}
			}
			if !found {
				t.Errorf("the create argv does not carry %s", tc.name)
			}
		})
	}

	// The verb itself, so a create that stopped being a create would not pass
	// on the strength of its flags alone.
	if len(create.Args) < 4 ||
		!create.Args[2].Equal(exec.MustFixed("workspace")) ||
		!create.Args[3].Equal(exec.MustFixed("create")) {
		t.Error("the create command is not `workspace create` after the global flags")
	}
}

// TestEveryCommandCarriesThePinAndTheIDFormat pins the two properties no single
// call site may forget.
//
// Both are checked over EVERY recorded command rather than over the one a given
// test cares about, because the failure they prevent is per-call-site: a verb
// that forgot the socket pin answers from whatever server the ambient
// environment selects, and a verb that forgot --id-format gets a reply whose
// KEYS are renamed.
func TestEveryCommandCarriesThePinAndTheIDFormat(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)
	ref, ok := res.Ref()
	if !ok {
		t.Fatalf("Start did not produce a reference: %v", res.Outcome())
	}
	a.Probe(context.Background(), ref)
	a.Close(context.Background(), ref)

	calls := run.calls()
	if len(calls) < 5 {
		t.Fatalf("expected readiness, snapshot, create, probe and close commands; got %d", len(calls))
	}
	// Built from the constructors rather than by calling pinnedEnv, and that is
	// not a style choice. Comparing against pinnedEnv(testSocket) made this
	// assertion self-referential: deleting the socket replacement from
	// pinnedEnv changed the expectation in step with the code, and the test
	// stayed green while every command lost its pin. Caught by mutation, not by
	// reading.
	wantEnv := []exec.EnvMutation{
		exec.ReplaceCmuxSocketPath(testSocket),
		exec.SetCmuxQuiet(),
	}
	for _, cmd := range calls {
		if len(cmd.Env) != len(wantEnv) {
			t.Fatalf("%v carried %d env mutations, want %d", cmd.Kind, len(cmd.Env), len(wantEnv))
		}
		for i := range wantEnv {
			if !cmd.Env[i].Equal(wantEnv[i]) {
				t.Errorf("%v: env mutation %d is not the pin", cmd.Kind, i)
			}
		}
		if len(cmd.Args) < 2 ||
			!cmd.Args[0].Equal(exec.MustFixed("--id-format")) ||
			!cmd.Args[1].Equal(exec.MustFixed("uuids")) {
			t.Errorf("%v does not lead with --id-format uuids; its reply's keys would be renamed", cmd.Kind)
		}
		isListing := cmd.Kind == exec.KindCmuxSnapshot || cmd.Kind == exec.KindCmuxReconcile || cmd.Kind == exec.KindCmuxProbe
		if isListing && cmd.StdoutMode != exec.CaptureCmuxWorkspaceList {
			t.Errorf("%v uses stdout mode %v, want streamed workspace projection", cmd.Kind, cmd.StdoutMode)
		}
		if !isListing && cmd.StdoutMode != exec.CaptureRaw {
			t.Errorf("%v uses stdout mode %v, want raw capture", cmd.Kind, cmd.StdoutMode)
		}
	}
}

// ---------------------------------------------------------------------------
// The --id-format trap, from both directions.
// ---------------------------------------------------------------------------

// TestACreateReplyWithoutAUUIDNeverBecomesAReference is the measured trap.
//
// Without the global --id-format flag, cmux names the create reply's field
// workspace_ref and fills it with "workspace:4" — a POSITIONAL INDEX that means
// a different workspace the moment anything below it closes. A parser keyed on
// workspace_id finds nothing; one that fell back to any ref-shaped key would
// bind a reference that later closes the wrong object.
//
// Both shapes must reconcile rather than produce a reference, and the fixtures
// are the exact envelopes cmux emits.
func TestACreateReplyWithoutAUUIDNeverBecomesAReference(t *testing.T) {
	cases := map[string]string{
		"the ref envelope the missing flag produces": `{"workspace_ref":"workspace:4","window_ref":"window:1"}`,
		"a workspace_id holding a ref":               `{"workspace_id":"workspace:4"}`,
		"an empty workspace_id":                      `{"workspace_id":""}`,
		"no id at all":                               `{"surface_id":"S"}`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			run := newRunner().reply1(exec.KindCmuxCreate, []byte(reply))
			a := newTestAdapter(t, run)
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if _, ok := res.Ref(); ok {
				t.Fatal("a create reply naming no UUID produced a reference")
			}
			if res.Outcome() == backend.RefKnown {
				t.Error("RefKnown from a reply that named no workspace")
			}
		})
	}
}

// TestACreateReplyNamingAUUIDBecomesThatExactReference is the acceptance case
// beside the refusals above: a refusal table with no accepting sibling cannot
// tell a working parser from one that refuses everything.
func TestACreateReplyNamingAUUIDBecomesThatExactReference(t *testing.T) {
	_, _, ref := startClean(t)

	id, err := ref.CMuxIdentity()
	if err != nil {
		t.Fatalf("CMuxIdentity: %v", err)
	}
	if id.Workspace() != workspaceA {
		t.Errorf("reference names workspace %q, want %q", id.Workspace(), workspaceA)
	}
	if ref.Kind() != backend.KindCmux {
		t.Errorf("reference kind = %v, want cmux", ref.Kind())
	}
}

// ---------------------------------------------------------------------------
// Fail-closed reading.
// ---------------------------------------------------------------------------

// TestAnUnreadableListingIsNeverAnEmptyOne is the contract internal/tmux states
// for its own parser and the tmux adapter had to be taught after shipping
// without it: a reply we could not read is not a reply saying "nothing here".
//
// Reporting absence from an unreadable listing is fail-OPEN in the direction
// that orphans a live surface — Start reports NotMutated so nothing is cleaned
// up, and Close reports AlreadyGone so a rollback obligation is discharged while
// still outstanding.
func TestAnUnreadableListingIsNeverAnEmptyOne(t *testing.T) {
	unreadable := map[string]exec.SensitiveResult{
		"not JSON at all": {Stdout: exec.BoundedOutputForTest([]byte("cmux: something went sideways"), exec.OutputComplete)},
		"truncated mid-document": {Stdout: exec.BoundedOutputForTest(
			listJSON([2]string{workspaceA, "x"}), exec.OutputOverflowed)},
		"rows present, none usable": {Stdout: exec.BoundedOutputForTest(
			listJSON([2]string{"workspace:4", "x"}, [2]string{"", "y"}), exec.OutputComplete)},
	}
	for name, reply := range unreadable {
		t.Run("start reconciles: "+name, func(t *testing.T) {
			run := newRunner().
				reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
				on(exec.KindCmuxReconcile, func() (exec.SensitiveResult, error) { return reply, nil })
			a := newTestAdapter(t, run)
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if res.Outcome() == backend.NotMutated {
				t.Error("an unreadable listing was reported as proof nothing was created")
			}
		})
		t.Run("close refuses: "+name, func(t *testing.T) {
			a, run, ref := startClean(t)
			run.on(exec.KindCmuxProbe, func() (exec.SensitiveResult, error) { return reply, nil })

			got := a.Close(context.Background(), ref)

			if got.State().SatisfiesRollback() {
				t.Error("an unreadable listing discharged a rollback obligation that is still outstanding")
			}
		})
	}
}

// TestACompleteEmptyListingIsARealAbsence is the acceptance case beside the
// refusals above. Without it, a parser that called every listing unreadable
// would pass the whole table and never conclude anything.
func TestACompleteEmptyListingIsARealAbsence(t *testing.T) {
	run := newRunner().
		reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
		reply1(exec.KindCmuxReconcile, listJSON())
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("a complete empty listing gave %v, want NotMutated", res.Outcome())
	}
}

// ---------------------------------------------------------------------------
// Readiness: the checks that run before anything is created.
// ---------------------------------------------------------------------------

// TestReadinessRefusesAServerAnsweringOnADifferentEndpoint pins the control tmux
// never had one for.
//
// cmux reports the socket_path it actually bound, so a reply naming a different
// path means the pin did not take and this launch is talking to a server it did
// not select. That matters more here than "wrong answers": the create argv
// carries the bootstrap, which carries the handshake socket path and the
// one-shot nonce. Without this check a wrong server receives them and answers
// perfectly well, so the failure is entirely silent.
func TestReadinessRefusesAServerAnsweringOnADifferentEndpoint(t *testing.T) {
	run := newRunner().reply1(exec.KindCmuxReadiness,
		capabilitiesJSON(wantProtocol, minVersion, "/tmp/somebody-elses/cmux.sock", requiredMethods))
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("disposition = %v, want NotMutated", res.Outcome())
	}
	if cls := causeClass(res); cls != backend.FailureIdentityMismatch {
		t.Errorf("class = %v, want FailureIdentityMismatch", cls)
	}
	if _, created := commandOfKind(run.calls(), exec.KindCmuxCreate); created {
		t.Error("a create ran against a server that answered on the wrong endpoint")
	}
}

// TestReadinessRefusesASocketItCannotProveIsOurs covers all three ownership
// arms with one criterion, because they are one control: the bootstrap is only
// safe to send to a socket this uid owns.
//
// Both operands are seamed for the reason the trampoline's are — planting a
// foreign-owned socket needs a second account, so without the seam the suite
// would be indifferent to whether the comparison exists at all.
func TestReadinessRefusesASocketItCannotProveIsOurs(t *testing.T) {
	cases := map[string]fakeInfo{
		"owned by another user":  {sys: &syscall.Stat_t{Ino: liveInode, Uid: testUID + 1}, mode: fs.ModeSocket | 0o600},
		"owner cannot be read":   {sys: nil, mode: fs.ModeSocket | 0o600},
		"not a socket at all":    {sys: &syscall.Stat_t{Ino: liveInode, Uid: testUID}, mode: 0o600},
		"a directory in its way": {sys: &syscall.Stat_t{Ino: liveInode, Uid: testUID}, mode: fs.ModeDir | 0o700},
	}
	for name, info := range cases {
		t.Run(name, func(t *testing.T) {
			run := newRunner()
			a := newTestAdapter(t, run, WithLstat(func(string) (os.FileInfo, error) { return info, nil }))
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if res.Outcome() != backend.NotMutated {
				t.Errorf("disposition = %v, want NotMutated", res.Outcome())
			}
			if len(run.calls()) != 0 {
				t.Error("a command was sent to a socket whose ownership could not be established")
			}
		})
	}
}

// TestReadinessRefusesAnIncompatibleServer covers protocol, version, and the
// advertised verb set with one criterion — the same reason the driven-backend
// table in the CLI shares one: a per-case assertion catches only per-case
// faults.
func TestReadinessRefusesAnIncompatibleServer(t *testing.T) {
	cases := map[string][]byte{
		"a different protocol on the same socket": capabilitiesJSON("herdr-socket", minVersion, testSocket, requiredMethods),
		"a protocol below the floor":              capabilitiesJSON(wantProtocol, minVersion-1, testSocket, requiredMethods),
		"create not advertised":                   capabilitiesJSON(wantProtocol, minVersion, testSocket, []string{"workspace.close", "workspace.list"}),
		"close not advertised":                    capabilitiesJSON(wantProtocol, minVersion, testSocket, []string{"workspace.create", "workspace.list"}),
		"list not advertised":                     capabilitiesJSON(wantProtocol, minVersion, testSocket, []string{"workspace.create", "workspace.close"}),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			run := newRunner().reply1(exec.KindCmuxReadiness, reply)
			a := newTestAdapter(t, run)
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if res.Outcome() != backend.NotMutated {
				t.Errorf("disposition = %v, want NotMutated", res.Outcome())
			}
			if cls := causeClass(res); cls != backend.FailureIncompatible {
				t.Errorf("class = %v, want FailureIncompatible", cls)
			}
			if _, created := commandOfKind(run.calls(), exec.KindCmuxCreate); created {
				t.Error("a create ran against an incompatible server")
			}
		})
	}
}

// TestAnInvalidPasswordIsItsOwnFailureClass binds the auth arm to the string
// cmux actually emits.
//
// The fixture is measured, not invented: on 0.64.22 every verb answers
// "Error: ERROR: Invalid password" with exit 1. The tmux adapter shipped a
// permission arm whose fixture was a string tmux never emits, so the right
// property was asserted against input that could not exercise it.
func TestAnInvalidPasswordIsItsOwnFailureClass(t *testing.T) {
	run := newRunner().on(exec.KindCmuxReadiness, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
				Stderr: exec.BoundedOutputForTest([]byte("Error: ERROR: Invalid password\n"), exec.OutputComplete),
			},
			exec.SensitiveErrorForTest(exec.KindCmuxReadiness, exec.OutcomeExit)
	})
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("disposition = %v, want NotMutated", res.Outcome())
	}
	if cls := causeClass(res); cls != backend.FailureAuthentication {
		t.Errorf("class = %v, want FailureAuthentication — an operator told "+
			"'unavailable' goes looking for a dead server instead of their password", cls)
	}
}

// TestClassifyRunErrorReadsTheSeamsSentinels pins that classification runs on
// the SEAM's vocabulary.
//
// *SensitiveError unwraps to its outcome's package sentinel and deliberately
// never to the underlying error, so a branch written against context.Canceled or
// os.ErrPermission is structurally unreachable in production — and a fake
// returning a bare errors.New would never notice, because it matches neither.
func TestClassifyRunErrorReadsTheSeamsSentinels(t *testing.T) {
	cases := map[exec.Outcome]backend.StartFailureClass{
		exec.OutcomeCanceled: backend.FailureCanceled,
		exec.OutcomeTimeout:  backend.FailureTimeout,
		exec.OutcomeInvalid:  backend.FailureInternal,
		exec.OutcomeExit:     backend.FailureUnavailable,
	}
	a := newTestAdapter(t, newRunner())
	empty := exec.SensitiveResult{}
	for outcome, want := range cases {
		got := a.classifyRunError(exec.SensitiveErrorForTest(exec.KindCmuxProbe, outcome), empty)
		if got.Class() != want {
			t.Errorf("outcome %v classified as %v, want %v", outcome, got.Class(), want)
		}
	}
}

// ---------------------------------------------------------------------------
// Reconciliation: zero, one, and the ambiguous many.
// ---------------------------------------------------------------------------

// TestReconciliationMatchesOnTheMarkerAndOnNovelty pins both halves of the
// match condition, and each case removes exactly one of them.
//
// Either condition alone is weaker than it looks. The marker alone cannot tell
// this attempt's workspace from a previous attempt's — a retry reuses the tag.
// Novelty alone would match any workspace the operator happened to open while
// the launch was in flight.
func TestReconciliationMatchesOnTheMarkerAndOnNovelty(t *testing.T) {
	tests := map[string]struct {
		before, after func(marker string) []byte
		want          backend.MutationOutcome
	}{
		"exactly one new marked workspace": {
			before: func(string) []byte { return listJSON() },
			after:  func(m string) []byte { return listJSON([2]string{workspaceA, m}) },
			want:   backend.RefKnown,
		},
		"a marked workspace that predates this attempt": {
			before: func(m string) []byte { return listJSON([2]string{workspaceA, m}) },
			after:  func(m string) []byte { return listJSON([2]string{workspaceA, m}) },
			want:   backend.NotMutated,
		},
		"a new workspace wearing somebody else's marker": {
			before: func(string) []byte { return listJSON() },
			after:  func(string) []byte { return listJSON([2]string{workspaceA, "fc-surface-notours"}) },
			want:   backend.NotMutated,
		},
		"a new workspace with no marker at all": {
			before: func(string) []byte { return listJSON() },
			after:  func(string) []byte { return listJSON([2]string{workspaceA, ""}) },
			want:   backend.NotMutated,
		},
		"two new workspaces wearing our marker": {
			before: func(string) []byte { return listJSON() },
			after: func(m string) []byte {
				return listJSON([2]string{workspaceA, m}, [2]string{workspaceB, m})
			},
			want: backend.OutcomeUnknown,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec, tag := newSpec(t)
			marker := tag.OwnershipName()
			run := newRunner().
				reply1(exec.KindCmuxSnapshot, tc.before(marker)).
				reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
				reply1(exec.KindCmuxReconcile, tc.after(marker))
			a := newTestAdapter(t, run)

			res := a.Start(context.Background(), spec)

			if res.Outcome() != tc.want {
				t.Errorf("disposition = %v, want %v", res.Outcome(), tc.want)
			}
		})
	}
}

// TestAnAmbiguousReconciliationCarriesTheTagAndNoReference states what
// OutcomeUnknown must and must not carry. A reference would invite the service
// to close one of an ambiguous pair, which is how a rollback destroys the wrong
// object; the tag is what lets an operator find both by hand.
func TestAnAmbiguousReconciliationCarriesTheTagAndNoReference(t *testing.T) {
	spec, tag := newSpec(t)
	marker := tag.OwnershipName()
	run := newRunner().
		reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
		reply1(exec.KindCmuxReconcile, listJSON([2]string{workspaceA, marker}, [2]string{workspaceB, marker}))
	a := newTestAdapter(t, run)

	res := a.Start(context.Background(), spec)

	if _, ok := res.Ref(); ok {
		t.Fatal("an ambiguous reconciliation produced a reference the service would close")
	}
	if got := mustTag(t, res); got.String() != tag.String() {
		t.Errorf("tag = %q, want %q", got.String(), tag.String())
	}
	readiness := 0
	for _, cmd := range run.calls() {
		if cmd.Kind == exec.KindCmuxReadiness {
			readiness++
		}
	}
	if readiness != 1 {
		t.Errorf("readiness calls = %d, want 1; ambiguity must not attempt a fresh bind", readiness)
	}
}

// TestReconciliationBindsABenignCtimeChangeToTheFreshIncarnation covers the
// decision in #367. The socket keeps its inode and only ctime moves, so the
// first reference correctly refuses the stale fingerprint. An exact new marker
// match then earns one fresh readiness pass and a closeable reference bound to
// the current incarnation.
func TestReconciliationBindsABenignCtimeChangeToTheFreshIncarnation(t *testing.T) {
	if !changeTimeSupported {
		t.Skip("socket ctime is not part of fingerprints on this platform")
	}
	spec, tag := newSpec(t)
	marker := tag.OwnershipName()

	stats := 0
	before := liveSocketWithChangeTime(liveInode, 1_000)
	after := liveSocketWithChangeTime(liveInode, 2_000)
	seam := func(path string) (os.FileInfo, error) {
		stats++
		if stats == 1 {
			return before(path)
		}
		return after(path)
	}
	var warnings bytes.Buffer
	run := newRunner().reply1(exec.KindCmuxReconcile, listJSON([2]string{workspaceA, marker}))
	a := newTestAdapter(t, run,
		WithLstat(seam),
		WithSocketDirStat(func(string) (os.FileInfo, error) {
			return fakeInfo{sys: &syscall.Stat_t{Uid: testUID}, mode: fs.ModeDir | 0o777}, nil
		}),
		WithWarnings(&warnings),
	)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.RefKnown || !res.Failed() {
		t.Fatalf("outcome = %v, failed = %v; want RefKnown-with-cause", res.Outcome(), res.Failed())
	}
	if _, ok := res.Ref(); !ok {
		t.Fatal("fresh reconciliation returned no closeable reference")
	}
	if got := strings.Count(warnings.String(), "warning: cmux socket directory"); got != 1 {
		t.Errorf("warning count = %d, want 1 across both readiness passes", got)
	}
}

func TestReconciliationKeepsOutcomeUnknownWhenFreshReadinessFails(t *testing.T) {
	spec, tag := newSpec(t)
	marker := tag.OwnershipName()
	readiness := 0
	run := newRunner().
		reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
		reply1(exec.KindCmuxReconcile, listJSON([2]string{workspaceA, marker})).
		on(exec.KindCmuxReadiness, func() (exec.SensitiveResult, error) {
			readiness++
			if readiness == 1 {
				return stdout(goodCapabilities()), nil
			}
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindCmuxReadiness, exec.OutcomeExit)
		})
	a := newTestAdapter(t, run)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.OutcomeUnknown {
		t.Errorf("outcome = %v, want OutcomeUnknown", res.Outcome())
	}
	if _, ok := res.Ref(); ok {
		t.Fatal("failed fresh readiness minted a reference")
	}
	if got := mustTag(t, res); got.String() != tag.String() {
		t.Errorf("tag = %q, want %q", got.String(), tag.String())
	}
}

func TestReconciliationNeverFollowsAChangedCmuxEndpoint(t *testing.T) {
	spec, tag := newSpec(t)
	marker := tag.OwnershipName()
	readiness := 0
	run := newRunner().
		reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
		reply1(exec.KindCmuxReconcile, listJSON([2]string{workspaceA, marker})).
		on(exec.KindCmuxReadiness, func() (exec.SensitiveResult, error) {
			readiness++
			socket := testSocket
			if readiness > 1 {
				socket = "/tmp/another-cmux/cmux.sock"
			}
			return stdout(capabilitiesJSON(wantProtocol, minVersion, socket, requiredMethods)), nil
		})
	a := newTestAdapter(t, run)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.OutcomeUnknown {
		t.Errorf("outcome = %v, want OutcomeUnknown", res.Outcome())
	}
	if _, ok := res.Ref(); ok {
		t.Fatal("a readiness reply naming another endpoint minted a reference")
	}
}

// TestAnAbsentServerProvesNothingWasCreated is the one listing failure that may
// conclude absence, and it is settled by a STAT rather than a diagnostic.
//
// cmux does emit a recognisable "Socket not found at <path>", but the tmux
// adapter was bitten twice classifying absence from diagnostic text: once
// reading a permission error as absence, once removing the arm and losing the
// case where absence is correct. A stat cannot be changed by a locale or a
// reworded release.
func TestAnAbsentServerProvesNothingWasCreated(t *testing.T) {
	failed := func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindCmuxReconcile, exec.OutcomeExit)
	}
	run := newRunner().
		reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
		on(exec.KindCmuxReconcile, failed)

	// The socket is present for readiness, then gone by the time reconciliation
	// asks — which is the sequence a cmux quit mid-launch actually produces.
	// Readiness takes exactly one stat, so every later one answers ENOENT.
	stats := 0
	live := liveSocket(liveInode)
	seam := func(p string) (os.FileInfo, error) {
		stats++
		if stats > 1 {
			return absentSocket(p)
		}
		return live(p)
	}
	a := newTestAdapter(t, run, WithLstat(seam))
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("disposition = %v, want NotMutated — an absent server is not hiding our workspace", res.Outcome())
	}
}

// ---------------------------------------------------------------------------
// Close and Probe.
// ---------------------------------------------------------------------------

// TestCloseTargetsTheExactUUID pins that the close operand is the reference's
// UUID and nothing else.
//
// cmux's other handles — a positional index, a `workspace:N` ref — name a
// DIFFERENT workspace once anything above them closes, so a rollback holding one
// destroys whatever slid into the slot.
func TestCloseTargetsTheExactUUID(t *testing.T) {
	a, run, ref := startClean(t)
	run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, ref.OwnershipName()}))

	got := a.Close(context.Background(), ref)

	if !got.State().SatisfiesRollback() {
		t.Fatalf("Close did not satisfy the rollback: %v", got)
	}
	cleanup, ok := commandOfKind(run.calls(), exec.KindCmuxCleanup)
	if !ok {
		t.Fatal("Close issued no cleanup command")
	}
	last := cleanup.Args[len(cleanup.Args)-1]
	if !last.Equal(exec.Opaque(workspaceA)) {
		t.Error("the close operand is not the reference's exact workspace UUID")
	}
}

// TestCloseAndProbeRefuseAReferenceForAnotherBackend pins the kind check.
//
// What it buys is the outcome STATE, not a prevented close: Ref.Validate already
// enforces that a reference's source matches its kind, so a foreign reference
// can never carry a cmux source and the source check would refuse it a line
// later regardless. The difference is between answering "unreadable" — we cannot
// speak to this object — and "identity mismatch", which is a claim about cmux
// identity that a tmux reference supports not at all.
func TestCloseAndProbeRefuseAReferenceForAnotherBackend(t *testing.T) {
	run := newRunner()
	a := newTestAdapter(t, run)
	foreign := foreignRef(t)

	closed := a.Close(context.Background(), foreign)
	if closed.State().SatisfiesRollback() {
		t.Error("a reference for another backend discharged a rollback obligation")
	}
	if probe := a.Probe(context.Background(), foreign); probe.State() == backend.ProbePresent {
		t.Error("a reference for another backend was reported present")
	}
	if len(run.calls()) != 0 {
		t.Error("a command reached cmux for a reference that is not a cmux reference")
	}
}

// TestARestartedServerIsAnIdentityMismatchNotAnAbsence pins the incarnation
// check, and the direction of its failure is the point.
//
// A restarted cmux answers a listing perfectly well; what it cannot do is hold
// the workspace this reference names. Without the check, a UUID the new server
// does not recognise reads as an ordinary absence — Close reports AlreadyGone
// and discharges a rollback obligation for a workspace that may still be open on
// the old incarnation.
func TestARestartedServerIsAnIdentityMismatchNotAnAbsence(t *testing.T) {
	a, run, ref := startClean(t)
	// A new inode is a new incarnation: the server rebound the same path.
	run.reply1(exec.KindCmuxProbe, listJSON())
	restarted := newTestAdapter(t, run, WithLstat(liveSocket(liveInode+1)))

	closed := restarted.Close(context.Background(), ref)
	if closed.State().SatisfiesRollback() {
		t.Error("a restarted server discharged a rollback obligation for a workspace it never held")
	}
	if probe := restarted.Probe(context.Background(), ref); probe.State() == backend.ProbePresent {
		t.Error("a restarted server reported the referenced workspace present")
	}
	if _, cleaned := commandOfKind(run.calls(), exec.KindCmuxCleanup); cleaned {
		t.Error("a close ran against a server that is not the one the reference was taken on")
	}
	_ = a
}

// TestAWorkspaceAlreadyGoneSatisfiesTheRollback covers both routes to that
// verdict with one criterion: the obligation was that the object be gone, and in
// both cases it is.
func TestAWorkspaceAlreadyGoneSatisfiesTheRollback(t *testing.T) {
	t.Run("absent from a complete listing", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindCmuxProbe, listJSON())

		if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
			t.Errorf("Close = %v, want a satisfied rollback", got)
		}
	})
	t.Run("the close itself races another", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, ref.OwnershipName()}))
		run.on(exec.KindCmuxCleanup, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{
					Stderr: exec.BoundedOutputForTest([]byte("Error: not_found: Workspace not found\n"), exec.OutputComplete),
				},
				exec.SensitiveErrorForTest(exec.KindCmuxCleanup, exec.OutcomeExit)
		})

		if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
			t.Errorf("Close = %v, want a satisfied rollback", got)
		}
	})
}

// TestAFailedCloseIsReportedAsAFailure is the acceptance case beside the two
// already-gone routes: without it, a Close that called every failure "already
// gone" would pass both and silently discharge every obligation.
func TestAFailedCloseIsReportedAsAFailure(t *testing.T) {
	a, run, ref := startClean(t)
	run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, ref.OwnershipName()}))
	run.on(exec.KindCmuxCleanup, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
				Stderr: exec.BoundedOutputForTest([]byte("Error: internal: something broke\n"), exec.OutputComplete),
			},
			exec.SensitiveErrorForTest(exec.KindCmuxCleanup, exec.OutcomeExit)
	})

	if got := a.Close(context.Background(), ref); got.State().SatisfiesRollback() {
		t.Errorf("Close = %v, want an unsatisfied rollback", got)
	}
}

// TestTheZeroLocateStateNeverClosesAndNeverReportsPresent pins the enum, which
// is the tested half of a guard whose default arm cannot be driven.
//
// The tmux adapter had this exact bug: with locateFound at zero, the default arm
// added to keep an unknown state away from the kill was unreachable for the very
// value it named — a zero-valued state selected `case locateFound:` and fell
// through, and Probe answered Present with Conclusive() true.
func TestTheZeroLocateStateNeverClosesAndNeverReportsPresent(t *testing.T) {
	var zero locateState
	if zero == locateFound {
		t.Fatal("the zero locateState is locateFound; every unknown-state guard " +
			"in this package sits on the wrong side of the value it names")
	}
	if zero != locateInvalid {
		t.Errorf("the zero locateState is %d, want locateInvalid", zero)
	}
}

// ---------------------------------------------------------------------------
// Endpoint resolution.
// ---------------------------------------------------------------------------

// TestResolveSocketRecordsTheChainThatChoseIt pins that the SOURCE differs by
// chain, not merely that a path came back.
//
// The distinction is what makes a reference portable: a reference taken against
// an explicitly pinned endpoint must not be answered by an adapter that resolved
// the default one, even on a machine where the two paths coincide today.
func TestResolveSocketRecordsTheChainThatChoseIt(t *testing.T) {
	tests := map[string]struct {
		env    map[string]string
		want   backend.ServerSource
		socket string
	}{
		"an explicit pin": {
			env:    map[string]string{"CMUX_SOCKET_PATH": testSocket},
			want:   backend.CmuxEnvServer(),
			socket: testSocket,
		},
		"the XDG state directory": {
			env:    map[string]string{"XDG_STATE_HOME": "/tmp/state"},
			want:   backend.CmuxDefaultServer(),
			socket: "/tmp/state/cmux/cmux.sock",
		},
		"the home-derived default": {
			env:    map[string]string{"HOME": "/tmp/home"},
			want:   backend.CmuxDefaultServer(),
			socket: "/tmp/home/.local/state/cmux/cmux.sock",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			socket, source, err := resolveSocket(func(k string) string { return tc.env[k] })
			if err != nil {
				t.Fatalf("resolveSocket: %v", err)
			}
			if socket != tc.socket {
				t.Errorf("socket = %q, want %q", socket, tc.socket)
			}
			if source != tc.want {
				t.Errorf("source = %v, want %v", source, tc.want)
			}
		})
	}
}

// TestResolveSocketRefusesAPathItCannotUse covers the shape rules. An
// unresolvable endpoint is refused at construction rather than producing an
// adapter whose every command fails later for a reason the operator cannot see.
func TestResolveSocketRefusesAPathItCannotUse(t *testing.T) {
	cases := map[string]map[string]string{
		"neither XDG_STATE_HOME nor HOME": {},
		"a relative pin":                  {"CMUX_SOCKET_PATH": "cmux.sock"},
		"an unclean pin":                  {"CMUX_SOCKET_PATH": "/tmp/../tmp/cmux.sock"},
		"a pin past sun_path":             {"CMUX_SOCKET_PATH": "/tmp/" + strings.Repeat("x", maxSocketPathLen) + "/cmux.sock"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := resolveSocket(func(k string) string { return env[k] })
			if !errors.Is(err, ErrResolveSocket) {
				t.Errorf("resolveSocket = %v, want ErrResolveSocket", err)
			}
		})
	}
}

// TestNewRefusesARelativeBinaryPath pins the reason the CLI does the PATH
// lookup: the sensitive runner will not let exec.LookPath choose the binary
// against the live process PATH, which is the one decision its captured
// environment would otherwise not cover.
func TestNewRefusesARelativeBinaryPath(t *testing.T) {
	getenv := func(string) string { return testSocket }
	if _, err := New(newRunner(), "cmux", getenv); !errors.Is(err, ErrResolveSocket) {
		t.Errorf("New with a relative path = %v, want ErrResolveSocket", err)
	}
	if _, err := New(nil, testCmux, getenv); !errors.Is(err, ErrResolveSocket) {
		t.Errorf("New with no runner = %v, want ErrResolveSocket", err)
	}
}

// TestKindReportsCmux is small but not trivial: surfaceAdapterFor routes on it,
// and an adapter reporting the wrong kind would be handed references belonging
// to another backend.
func TestKindReportsCmux(t *testing.T) {
	if got := newTestAdapter(t, newRunner()).Kind(); got != backend.KindCmux {
		t.Errorf("Kind() = %v, want cmux", got)
	}
}

// TestParseCreatedWorkspaceRefusesEveryNonUUID tests the parser DIRECTLY, and
// the reason is a mutation result rather than a preference.
//
// Going through Start could not tell this control from its absence: with the
// UUID check deleted, parseCreatedWorkspace happily returns "workspace:4" and
// the reference builder refuses it one call later, so every end-to-end
// assertion stayed green. That is a staged pair where the second layer is doing
// the work, and the first is only load-bearing for the truncation case it also
// owns. Testing the pure function separately is what makes each layer visible.
func TestParseCreatedWorkspaceRefusesEveryNonUUID(t *testing.T) {
	refusals := map[string]exec.SensitiveResult{
		"the ref envelope the missing flag produces": stdout([]byte(`{"workspace_ref":"workspace:4"}`)),
		"a workspace_id holding a ref":               stdout([]byte(`{"workspace_id":"workspace:4"}`)),
		"an empty workspace_id":                      stdout([]byte(`{"workspace_id":""}`)),
		"a truncated but still parseable document": {Stdout: exec.BoundedOutputForTest(
			createJSON(workspaceA), exec.OutputOverflowed)},
		"not JSON at all": stdout([]byte("OK workspace:2")),
	}
	for name, res := range refusals {
		t.Run(name, func(t *testing.T) {
			if got, err := parseCreatedWorkspace(res.Stdout); err == nil {
				t.Errorf("parseCreatedWorkspace returned %q, want a refusal", got)
			}
		})
	}
	t.Run("a valid uuid, returned unchanged", func(t *testing.T) {
		got, err := parseCreatedWorkspace(stdout(createJSON(workspaceA)).Stdout)
		if err != nil {
			t.Fatalf("parseCreatedWorkspace: %v", err)
		}
		if got != workspaceA {
			t.Errorf("parseCreatedWorkspace = %q, want %q unchanged", got, workspaceA)
		}
	})
}

// TestATruncatedCapabilitiesReplyIsNotAReadyServer covers readiness's own
// completeness guard.
//
// The fixture is a COMPLETE, valid document flagged as overflowed, which is the
// only shape that can exercise it: a document truncated mid-object fails to
// parse and would be caught by the JSON error instead, so a naive fixture tests
// the wrong guard and the real one survives its mutation.
func TestATruncatedCapabilitiesReplyIsNotAReadyServer(t *testing.T) {
	run := newRunner().on(exec.KindCmuxReadiness, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
			Stdout: exec.BoundedOutputForTest(goodCapabilities(), exec.OutputOverflowed),
		}, nil
	})
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("outcome = %v, want NotMutated", res.Outcome())
	}
	if _, created := commandOfKind(run.calls(), exec.KindCmuxCreate); created {
		t.Error("a create ran on the strength of a readiness reply that was cut short")
	}
}

// TestAnUnreadableOwnerRefusesEvenWhenTheUIDsWouldHaveMatched isolates the
// cannot-establish arm from the comparison beside it.
//
// The two look redundant and are not. With the platform unable to report an
// owner, OwnerUID yields zero — so under any non-root uid the comparison
// happens to refuse anyway, and deleting the cannot-establish arm changes
// nothing. Running as uid 0 is what separates them: the zero owner then MATCHES,
// and only the explicit arm still refuses. Without this the guard would be
// untested for precisely the account where it matters most.
func TestAnUnreadableOwnerRefusesEvenWhenTheUIDsWouldHaveMatched(t *testing.T) {
	unreadable := fakeInfo{sys: nil, mode: fs.ModeSocket | 0o600}
	run := newRunner()
	a := newTestAdapter(t, run,
		WithLstat(func(string) (os.FileInfo, error) { return unreadable, nil }),
		WithSelfUID(func() int { return 0 }),
	)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("outcome = %v, want NotMutated", res.Outcome())
	}
	if causeClass(res) != backend.FailurePermissionDenied {
		t.Errorf("class = %v, want FailurePermissionDenied", causeClass(res))
	}
	if len(run.calls()) != 0 {
		t.Error("a command was sent to a socket whose owner could not be established")
	}
}

// TestSocketDirectoryWarningIsAdvisory pins Cameron's ruling on #369: cmux
// chooses its socket location, and forgectl makes a known local disruption
// risk visible without turning it into a readiness refusal.
func TestSocketDirectoryWarningIsAdvisory(t *testing.T) {
	tests := map[string]struct {
		stat func(string) (os.FileInfo, error)
		want string
	}{
		"group writable": {
			stat: func(string) (os.FileInfo, error) {
				return fakeInfo{sys: &syscall.Stat_t{Uid: testUID}, mode: fs.ModeDir | 0o720}, nil
			},
			want: "warning: cmux socket directory \"/tmp/cmuxtest\" is group or world writable; " +
				"forgectl will honor this location, but another local user may disrupt launches\n",
		},
		"foreign owned": {
			stat: func(string) (os.FileInfo, error) {
				return fakeInfo{sys: &syscall.Stat_t{Uid: testUID + 1}, mode: fs.ModeDir | 0o700}, nil
			},
			want: "warning: cmux socket directory \"/tmp/cmuxtest\" is owned by another user; " +
				"forgectl will honor this location, but another local user may disrupt launches\n",
		},
		"private": {
			stat: func(string) (os.FileInfo, error) {
				return fakeInfo{sys: &syscall.Stat_t{Uid: testUID}, mode: fs.ModeDir | 0o700}, nil
			},
		},
		"directory cannot be inspected": {
			stat: func(string) (os.FileInfo, error) { return nil, os.ErrPermission },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var warnings bytes.Buffer
			run := newRunner()
			a := newTestAdapter(t, run, WithSocketDirStat(tt.stat), WithWarnings(&warnings))
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if res.Outcome() != backend.RefKnown {
				t.Errorf("outcome = %v, want RefKnown; warning must not block readiness", res.Outcome())
			}
			if got := warnings.String(); got != tt.want {
				t.Errorf("warning = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAServerRestartingMidCreateNeverBecomesAReference pins the re-fingerprint
// that brackets the mutation.
//
// cmux reports no server pid and no start time, so the socket inode is the only
// witness to a restart — unlike tmux, which carries three. Taking the
// fingerprint again after the create is what converts "the server turned over
// while we were creating, and we cannot say which incarnation holds this
// workspace" from a silent mixed state into a refusal. Without it, Start hands
// back a reference bound to an incarnation that no longer exists, and the
// rollback that follows closes a UUID on the wrong server or nothing at all.
func TestAServerRestartingMidCreateNeverBecomesAReference(t *testing.T) {
	// Readiness takes the first stat; every stat after it reports a new inode,
	// which is what a server rebinding the same path looks like.
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
	a := newTestAdapter(t, run, WithLstat(seam))
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if _, ok := res.Ref(); ok {
		t.Fatal("a reference was minted across a server restart; a later close would " +
			"target a UUID on an incarnation that never held it")
	}
}

// TestAForeignReferenceIsRefusedAsUnreadableNotAsAMismatch pins what the kind
// check BUYS, which is the outcome state and not a prevented close.
//
// Asserting only "nothing was closed" could not see this control at all:
// Ref.Validate forbids a foreign source on a given kind, the source check
// refuses a line later, and the identity accessor refuses after that — three
// layers, and removing all of them still closes nothing. What differs between
// them is the verdict. Kind mismatch means "we cannot speak to this object";
// FailureIdentityMismatch would instead be a claim about cmux identity that a
// tmux reference supports not at all, and would send an operator looking for a
// restarted server that never existed.
func TestAForeignReferenceIsRefusedAsUnreadableNotAsAMismatch(t *testing.T) {
	a := newTestAdapter(t, newRunner())
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
}

// TestACmuxReferenceFromAnotherSelectionChainIsAMismatch drives the source
// check on its own.
//
// It needs a reference that is genuinely cmux — so the kind check passes and
// the identity accessor answers — but was taken through the OTHER endpoint
// chain. That is what separates this control from the kind check above, and it
// is a real scenario rather than a contrived one: a reference taken while
// CMUX_SOCKET_PATH was set must not be answered by an adapter that resolved the
// default endpoint, even on a machine where the two paths coincide today.
func TestACmuxReferenceFromAnotherSelectionChainIsAMismatch(t *testing.T) {
	_, _, ref := startClean(t) // taken through the env chain
	other := rebindSource(t, ref, backend.CmuxDefaultServer())

	run := newRunner()
	a := newTestAdapter(t, run) // still pinned through the env chain

	closed := a.Close(context.Background(), other)
	if closed.State() != backend.CloseIdentityMismatch {
		t.Errorf("Close state = %v, want identity-mismatch", closed.State())
	}
	if probe := a.Probe(context.Background(), other); probe.State() != backend.ProbeIdentityMismatch {
		t.Errorf("Probe state = %v, want identity-mismatch", probe.State())
	}
	if len(run.calls()) != 0 {
		t.Error("a command reached cmux for a reference from a different selection chain")
	}
}

// TestCloseRefusesAWorkspaceThatIsNotOurs is the ownership read-back the Closer
// contract states as a MUST, and it is the one test in this file guarding
// against the worst outcome the package can produce.
//
// The obligation exists because the type system cannot carry it for this
// backend. A tmux session name is CHOSEN by forgectl, so Ref.Validate binds it
// to the ownership tag; a cmux workspace UUID is server-assigned and carries
// nothing of ours. So a create response naming the wrong object — a raced
// reply, a stale id echoed after a restart — yields a fully VALID Ref pointing
// at somebody else's workspace, and every check upstream of the read-back
// passes it: the kind matches, the source matches, the incarnation matches, and
// the UUID is present in the listing.
//
// Without the read-back, Close issues `workspace close <operator's UUID>` and
// reports success. Not a failed rollback — a rollback that destroys something
// it never created.
//
// The fixture is the whole point. Every other Close/Probe test in this file
// feeds ref.OwnershipName() as the description, so the suite LOOKED like it
// covered ownership while nothing asserted on it; those fixtures could not have
// gone red no matter what locate did.
func TestCloseRefusesAWorkspaceThatIsNotOurs(t *testing.T) {
	a, run, ref := startClean(t)
	// Same UUID the reference names, wearing somebody else's marker.
	run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, "fc-surface-somebodyelse"}))

	closed := a.Close(context.Background(), ref)
	if closed.State() != backend.CloseIdentityMismatch {
		t.Errorf("Close state = %v, want identity-mismatch", closed.State())
	}
	if closed.State().SatisfiesRollback() {
		t.Error("closing somebody else's workspace was reported as a satisfied rollback")
	}
	if _, cleaned := commandOfKind(run.calls(), exec.KindCmuxCleanup); cleaned {
		t.Fatal("a close command was issued against a workspace carrying another owner's marker")
	}

	if probe := a.Probe(context.Background(), ref); probe.State() != backend.ProbeIdentityMismatch {
		t.Errorf("Probe state = %v, want identity-mismatch", probe.State())
	}
}

// TestCloseAcceptsOnlyTheExactMarker is the acceptance side of the read-back,
// plus the near-misses. A comparison written with a prefix or a fold would pass
// the refusal above and fail here.
func TestCloseAcceptsOnlyTheExactMarker(t *testing.T) {
	// Each case DERIVES its planted marker from the reference under test rather
	// than from a literal captured earlier, and that is the whole reason these
	// cases test anything.
	//
	// The first version of this table built its strings from an outer
	// startClean's marker while every subtest minted a fresh reference with a
	// fresh random tag — so "case-folded" was not a case-folded marker, it was
	// an unrelated tag, refused for a reason that had nothing to do with the
	// property named. Every case passed under an EqualFold comparison the table
	// was written to reject. A refusal fixture caught by an earlier rule tests
	// nothing, and the name is what conceals it.
	refusals := map[string]func(marker string) string{
		"empty":              func(string) string { return "" },
		"a prefix of ours":   func(m string) string { return m[:len(m)-1] },
		"ours with a suffix": func(m string) string { return m + "x" },
		"case-folded":        strings.ToUpper,
		"another launch's":   func(string) string { return "fc-surface-" + strings.Repeat("a", 32) },
	}
	for name, plant := range refusals {
		t.Run("refuses "+name, func(t *testing.T) {
			a, run, ref := startClean(t)
			planted := plant(ref.OwnershipName())
			if planted == ref.OwnershipName() {
				t.Fatalf("the fixture equals the real marker; this case could not fail")
			}
			run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, planted}))

			if got := a.Close(context.Background(), ref); got.State() != backend.CloseIdentityMismatch {
				t.Errorf("Close state = %v, want identity-mismatch", got.State())
			}
			if _, cleaned := commandOfKind(run.calls(), exec.KindCmuxCleanup); cleaned {
				t.Error("a close command was issued for a marker that is not ours")
			}
		})
	}

	t.Run("accepts the exact marker", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, ref.OwnershipName()}))

		if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
			t.Fatalf("Close = %v, want a satisfied rollback", got)
		}
		if _, cleaned := commandOfKind(run.calls(), exec.KindCmuxCleanup); !cleaned {
			t.Error("no close command was issued for a workspace that is ours")
		}
	})
}

// TestOnlyAnAbsentEndpointReadsAsGone pins the narrowest reading of a readiness
// failure.
//
// Every readiness failure that is NOT an absent socket must be unreadable. The
// mutation that made this arm unconditional survived the first sweep, and what
// it would have cost is the whole rollback: an auth failure, an incompatible
// server, or a socket we cannot prove is ours would all have reported
// AlreadyGone for a surface that is still running with the harness inside it.
func TestOnlyAnAbsentEndpointReadsAsGone(t *testing.T) {
	authRefusal := func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
				Stderr: exec.BoundedOutputForTest([]byte("Error: ERROR: Invalid password\n"), exec.OutputComplete),
			},
			exec.SensitiveErrorForTest(exec.KindCmuxReadiness, exec.OutcomeExit)
	}
	t.Run("an auth failure is unreadable, not gone", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner().on(exec.KindCmuxReadiness, authRefusal)
		a := newTestAdapter(t, run)

		closed := a.Close(context.Background(), ref)
		if closed.State().SatisfiesRollback() {
			t.Error("an auth failure discharged the rollback; the surface would be orphaned")
		}
		if probe := a.Probe(context.Background(), ref); probe.State() != backend.ProbeUnreadable {
			t.Errorf("Probe state = %v, want unreadable", probe.State())
		}
	})

	t.Run("an absent endpoint is gone", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner().on(exec.KindCmuxReadiness, authRefusal)
		a := newTestAdapter(t, run, WithLstat(absentSocket))

		if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
			t.Errorf("Close = %v, want a satisfied rollback for an endpoint that is not there", got)
		}
	})
}

// TestTheCloseOperandComesFromTheListing pins which spelling of the identifier
// reaches cmux.
//
// The listing's, not the reference's — because the map key is case-folded while
// the value is not, and the whole reason for that split is that a Close which
// cannot find its own workspace reports it already gone. A mutation swapping
// row.id for the reference's spelling survived the first sweep, leaving the
// protection the code documents unexercised.
func TestTheCloseOperandComesFromTheListing(t *testing.T) {
	_, _, ref := startClean(t)
	lower := strings.ToLower(workspaceA)
	if lower == workspaceA {
		t.Fatal("the fixture UUID has no case to differ in; this test could not fail")
	}

	run := newRunner().reply1(exec.KindCmuxProbe, listJSON([2]string{lower, ref.OwnershipName()}))
	a := newTestAdapter(t, run)

	if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
		t.Fatalf("Close = %v; a differently-cased listing lost track of its own workspace", got)
	}
	cleanup, ok := commandOfKind(run.calls(), exec.KindCmuxCleanup)
	if !ok {
		t.Fatal("Close issued no cleanup command")
	}
	if last := cleanup.Args[len(cleanup.Args)-1]; !last.Equal(exec.Opaque(lower)) {
		t.Error("the close operand is not the spelling the listing used")
	}
}

// TestATruncatedRefusalIsNotAlreadyGone pins the completeness check on the
// already-gone arm.
//
// A truncated stderr whose visible prefix happens to contain `not_found:` would
// otherwise discharge a rollback on the strength of a stream we did not finish
// reading — the same fail-open shape as reading a truncated listing as empty.
func TestATruncatedRefusalIsNotAlreadyGone(t *testing.T) {
	a, run, ref := startClean(t)
	run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, ref.OwnershipName()}))
	run.on(exec.KindCmuxCleanup, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
				Stderr: exec.BoundedOutputForTest(
					[]byte("Error: not_found: Workspace not f"), exec.OutputOverflowed),
			},
			exec.SensitiveErrorForTest(exec.KindCmuxCleanup, exec.OutcomeExit)
	})

	if got := a.Close(context.Background(), ref); got.State().SatisfiesRollback() {
		t.Errorf("Close = %v; a stream we could not finish reading discharged the rollback", got)
	}
}

// TestAnInvalidPasswordOnStdoutIsAlsoAnAuthFailure covers the second stream.
//
// Measured on 0.64.22, cmux puts this refusal on STDERR — the sibling test uses
// that stream. Both are searched because the classification is advisory and a
// false negative sends an operator hunting a dead server instead of their
// password, so the arm should not be coupled to which stream a future release
// picks. This is the test that makes that second stream a real control rather
// than an untested branch.
func TestAnInvalidPasswordOnStdoutIsAlsoAnAuthFailure(t *testing.T) {
	run := newRunner().on(exec.KindCmuxReadiness, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{
				Stdout: exec.BoundedOutputForTest([]byte("Error: ERROR: Invalid password\n"), exec.OutputComplete),
			},
			exec.SensitiveErrorForTest(exec.KindCmuxReadiness, exec.OutcomeExit)
	})
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if causeClass(res) != backend.FailureAuthentication {
		t.Errorf("class = %v, want FailureAuthentication", causeClass(res))
	}
}

// TestProbeAnswersItsTwoPositiveOutcomes covers the arms Prober exists FOR.
//
// Every other Probe assertion in this file is a refusal — unreadable, identity
// mismatch, not-present — so the two arms that actually answer the question had
// no accepting sibling and both their mutants survived. Swapping absent for
// present is the fail-OPEN direction, and it is precisely what Probe's own
// default arm is written to avoid; a refusal-only table could not see either.
func TestProbeAnswersItsTwoPositiveOutcomes(t *testing.T) {
	t.Run("present when the marked workspace is listed", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, ref.OwnershipName()}))

		if got := a.Probe(context.Background(), ref); got.State() != backend.ProbePresent {
			t.Errorf("Probe state = %v, want present", got.State())
		}
	})
	t.Run("gone on a complete empty listing", func(t *testing.T) {
		a, run, ref := startClean(t)
		run.reply1(exec.KindCmuxProbe, listJSON())

		if got := a.Probe(context.Background(), ref); got.State() != backend.ProbeGone {
			t.Errorf("Probe state = %v, want gone", got.State())
		}
	})
}

// TestAPositiveReconciliationBindsToTheFreshIncarnation covers the guarded
// fresh-bind ruling with the stronger inode-change witness.
//
// Reconciliation finds exactly one workspace carrying our marker and absent
// from the pre-snapshot — so something was definitely created — and then
// building the reference fails because the mid-create re-fingerprint catches
// an incarnation change. Full readiness on the original endpoint then binds
// the exact match to the fresh incarnation instead of retrying the stale one.
func TestAPositiveReconciliationBindsToTheFreshIncarnation(t *testing.T) {
	spec, tag := newSpec(t)
	marker := tag.OwnershipName()

	// The socket turns over after readiness, so reference's incarnation check
	// refuses while the listing still shows our workspace.
	stats := 0
	before, after := liveSocket(liveInode), liveSocket(liveInode+1)
	seam := func(p string) (os.FileInfo, error) {
		stats++
		if stats == 1 {
			return before(p)
		}
		return after(p)
	}
	run := newRunner().
		reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
		reply1(exec.KindCmuxReconcile, listJSON([2]string{workspaceA, marker}))
	a := newTestAdapter(t, run, WithLstat(seam))

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.RefKnown || !res.Failed() {
		t.Errorf("outcome = %v, failed = %v; want RefKnown-with-cause", res.Outcome(), res.Failed())
	}
	ref, ok := res.Ref()
	if !ok {
		t.Fatal("an exact positive reconciliation returned no reference")
	}
	if ref.Tag().String() != tag.String() {
		t.Errorf("ref tag = %q, want %q", ref.Tag().String(), tag.String())
	}
}

// TestAFailedPreSnapshotStopsTheLaunch pins the check that makes reconciliation
// able to tell this attempt's workspace from the last one's.
//
// Without it, a listing failure yields a nil pre-snapshot and every workspace
// looks novel. On a RETRY — which reuses the same tag, and is exactly what the
// pre-snapshot is documented to disambiguate — a leftover from the previous
// attempt then matches as new: two matches if it is still there, or a reference
// bound to the wrong workspace if the first was already cleaned.
func TestAFailedPreSnapshotStopsTheLaunch(t *testing.T) {
	run := newRunner().on(exec.KindCmuxSnapshot, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindCmuxSnapshot, exec.OutcomeExit)
	})
	a := newTestAdapter(t, run)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Outcome() != backend.NotMutated {
		t.Errorf("outcome = %v, want NotMutated", res.Outcome())
	}
	if _, created := commandOfKind(run.calls(), exec.KindCmuxCreate); created {
		t.Error("a create ran without a pre-snapshot to reconcile against")
	}
}

// TestACloseAgainstAVanishedServerSatisfiesTheRollback covers the other half of
// the already-gone condition.
//
// The sibling test drives the `not_found:` half; this one drives the stat half,
// which had no fixture. The close command itself fails while the endpoint is
// gone — a cmux quitting mid-rollback — and the obligation is still discharged,
// because the object being gone is the whole obligation.
func TestACloseAgainstAVanishedServerSatisfiesTheRollback(t *testing.T) {
	_, _, ref := startClean(t)

	// Present for readiness, absent by the time the failed close asks. Close
	// takes exactly two stats — readiness's ownership check, then serverGone —
	// so the boundary is after the first.
	stats := 0
	live := liveSocket(liveInode)
	seam := func(p string) (os.FileInfo, error) {
		stats++
		if stats > 1 {
			return absentSocket(p)
		}
		return live(p)
	}
	run := newRunner().reply1(exec.KindCmuxProbe, listJSON([2]string{workspaceA, ref.OwnershipName()}))
	run.on(exec.KindCmuxCleanup, func() (exec.SensitiveResult, error) {
		return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindCmuxCleanup, exec.OutcomeExit)
	})
	a := newTestAdapter(t, run, WithLstat(seam))

	if got := a.Close(context.Background(), ref); !got.State().SatisfiesRollback() {
		t.Errorf("Close = %v, want a satisfied rollback against a server that is gone", got)
	}
}

// TestAnAbsentSocketOnlyProvesAbsenceForTheRightFailure pins the CLASS half of
// both absence gates, which the stat half alone cannot reach.
//
// Absence is concluded from two facts together: the failure was
// FailureUnavailable, and the socket is not there. Only the pair means "no
// server". Either alone is a different claim — a malformed reply or a rejected
// credential says nothing about whether the workspace exists, and the socket
// being gone at the moment we happen to look is a race, not a verdict.
//
// The two arms need different setups because they fail at different depths, and
// that is why the class gate is only reachable through a race here: whenever the
// socket is absent from the start, readiness fails Unavailable at its own stat
// before any runner call, so the gate passes for the right reason. Driving the
// refusing direction means a socket that is present when readiness looks and
// gone when the absence check does.
func TestAnAbsentSocketOnlyProvesAbsenceForTheRightFailure(t *testing.T) {
	// The socket is live for the first stat and gone for every one after, which
	// is the race both gates exist to survive.
	vanishing := func() func(string) (os.FileInfo, error) {
		stats := 0
		live := liveSocket(liveInode)
		return func(p string) (os.FileInfo, error) {
			stats++
			if stats > 1 {
				return absentSocket(p)
			}
			return live(p)
		}
	}

	t.Run("reconcile: a malformed listing is not proof nothing was created", func(t *testing.T) {
		spec, _ := newSpec(t)
		run := newRunner().
			reply1(exec.KindCmuxCreate, []byte(`{"workspace_id":""}`)).
			reply1(exec.KindCmuxReconcile, []byte("cmux: not json at all"))
		a := newTestAdapter(t, run, WithLstat(vanishing()))

		res := a.Start(context.Background(), spec)

		if res.Outcome() == backend.NotMutated {
			t.Error("a listing we could not PARSE was read as proof nothing was created, " +
				"because the socket happened to be gone when we looked")
		}
	})

	t.Run("locate: an auth failure is not proof the workspace is gone", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner().on(exec.KindCmuxReadiness, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{
					Stderr: exec.BoundedOutputForTest([]byte("Error: ERROR: Invalid password\n"), exec.OutputComplete),
				},
				exec.SensitiveErrorForTest(exec.KindCmuxReadiness, exec.OutcomeExit)
		})
		a := newTestAdapter(t, run, WithLstat(vanishing()))

		if got := a.Close(context.Background(), ref); got.State().SatisfiesRollback() {
			t.Error("a rejected credential discharged the rollback because the socket " +
				"vanished between readiness and the absence check")
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// rebindSource rebuilds a cmux reference against a different server source,
// keeping every other field. It goes through the public constructor so the
// result is a reference Ref.Validate accepts — a hand-built struct would prove
// nothing about a shape production can produce.
func rebindSource(t *testing.T, ref backend.Ref, source backend.ServerSource) backend.Ref {
	t.Helper()
	id, err := ref.CMuxIdentity()
	if err != nil {
		t.Fatalf("CMuxIdentity: %v", err)
	}
	out, err := backend.NewCmuxRef(source, ref.Server(), ref.Tag(), id)
	if err != nil {
		t.Fatalf("NewCmuxRef: %v", err)
	}
	return out
}

func commandOfKind(calls []exec.SensitiveCommand, kind exec.CommandKind) (exec.SensitiveCommand, bool) {
	for _, c := range calls {
		if c.Kind == kind {
			return c, true
		}
	}
	return exec.SensitiveCommand{}, false
}

// foreignRef builds a valid reference belonging to a different backend.
func foreignRef(t *testing.T) backend.Ref {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatalf("NewRecoveryTag: %v", err)
	}
	server, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/tmp/tmux-501/default",
		Version:  "tmux 3.7b",
		Inode:    liveInode,
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

// causeClass reads a result's failure class, treating an absent cause as the
// unspecified class rather than panicking — a test asserting on a class wants a
// mismatch reported, not a nil deref.
func causeClass(res backend.StartResult) backend.StartFailureClass {
	cause, ok := res.Cause()
	if !ok {
		return backend.FailureUnspecified
	}
	return cause.Class()
}

// mustTag reads the recovery tag an OutcomeUnknown must carry.
func mustTag(t *testing.T, res backend.StartResult) backend.RecoveryTag {
	t.Helper()
	tag, ok := res.RecoveryTag()
	if !ok {
		t.Fatal("the result carries no recovery tag; an operator has no handle on what was left behind")
	}
	return tag
}
