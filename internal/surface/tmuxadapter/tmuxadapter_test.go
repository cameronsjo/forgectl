package tmuxadapter

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

const (
	testSocket = "/tmp/tmux-501/default"
	testTmux   = "/opt/homebrew/bin/tmux"
	testCWD    = "/Users/example/code/forgectl"
	liveInode  = 4242
)

// fakeInfo is the minimum os.FileInfo the fingerprint path reads. Sys() returns
// nil rather than a *syscall.Stat_t so fillStat's type assertion fails — which
// is the point for the tests that need fingerprinting to fail closed; the ones
// that need it to succeed use statInfo below.
type fakeInfo struct{ sys any }

func (fakeInfo) Name() string       { return "default" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() fs.FileMode  { return fs.ModeSocket | 0o600 }
func (fakeInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any         { return f.sys }

// statFor builds the platform stat a fingerprint reads. Only Ino is set: it is
// uint64 on both Darwin and Linux, whereas Dev is int32 on one and uint64 on
// the other — and Dev is not what the fingerprint depends on.
func statFor(inode uint64) *syscall.Stat_t { return &syscall.Stat_t{Ino: inode} }

// liveSocket is an lstat seam reporting a socket with a usable inode, so
// Fingerprint's non-zero-inode requirement is met.
func liveSocket(inode uint64) func(string) (os.FileInfo, error) {
	return func(string) (os.FileInfo, error) { return fakeInfo{sys: statFor(inode)}, nil }
}

func newTestAdapter(t *testing.T, run exec.SensitiveRunner, env map[string]string, opts ...Option) *Adapter {
	t.Helper()
	getenv := func(k string) string { return env[k] }
	a, err := New(run, testTmux, getenv, func() int { return 501 }, opts...)
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
	boot, err := backend.NewBootstrapCommand(exec.Opaque("forgectl surface _exec --protocol 1"))
	if err != nil {
		t.Fatalf("NewBootstrapCommand: %v", err)
	}
	spec, err := backend.NewStartSpec(testCWD, "forgectl", tag, boot)
	if err != nil {
		t.Fatalf("NewStartSpec: %v", err)
	}
	return spec, tag
}

// row renders one -F line in the shape identityFormat asks for.
func row(pid, start, id, name string) string {
	return strings.Join([]string{pid, start, id, name}, tmux.FieldSep)
}

func stdout(s string) exec.SensitiveResult {
	return exec.SensitiveResult{
		Stdout: exec.BoundedOutputForTest([]byte(s), exec.OutputComplete),
		Stderr: exec.BoundedOutputForTest(nil, exec.OutputComplete),
	}
}

func stderr(s string) exec.SensitiveResult {
	return exec.SensitiveResult{
		Stdout: exec.BoundedOutputForTest(nil, exec.OutputComplete),
		Stderr: exec.BoundedOutputForTest([]byte(s), exec.OutputComplete),
	}
}

// scripted answers each command kind, so a test states only the kinds it cares
// about and every other kind gets a sane default.
type scripted struct {
	version string
	byKind  map[exec.CommandKind]func() (exec.SensitiveResult, error)
}

func (s scripted) runFunc(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
	if fn, ok := s.byKind[cmd.Kind]; ok {
		return fn()
	}
	if cmd.Kind == exec.KindTmuxReadiness {
		v := s.version
		if v == "" {
			v = "tmux 3.7b"
		}
		return stdout(v), nil
	}
	return stdout(""), nil
}

func TestResolveSocketRecordsTheChainThatChoseIt(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantSocket string
		wantSource backend.ServerSource
	}{
		{
			// $TMUX wins: it names the server the operator is actually looking
			// at, and its value is <socket>,<pid>,<session>.
			name:       "inherited TMUX",
			env:        map[string]string{"TMUX": "/tmp/tmux-501/default,91,0"},
			wantSocket: "/tmp/tmux-501/default",
			wantSource: backend.TmuxCurrentServer(),
		},
		{
			name:       "derived default",
			env:        nil,
			wantSocket: "/tmp/tmux-501/default",
			wantSource: backend.TmuxDefaultServer(),
		},
		{
			name:       "TMUX_TMPDIR moves the default",
			env:        map[string]string{"TMUX_TMPDIR": "/private/run"},
			wantSocket: "/private/run/tmux-501/default",
			wantSource: backend.TmuxDefaultServer(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter(t, &exec.FakeSensitiveRunner{}, tt.env)
			if a.socket != tt.wantSocket {
				t.Errorf("socket = %q, want %q", a.socket, tt.wantSocket)
			}
			// The SOURCE is what travels in a reference; the path never does.
			// Two chains that happen to resolve to the same file today are
			// still different chains.
			if a.source != tt.wantSource {
				t.Errorf("source = %v, want %v", a.source, tt.wantSource)
			}
		})
	}
}

func TestNewRefusesAnUnusableSocketPath(t *testing.T) {
	for _, tt := range []struct{ name, tmuxEnv string }{
		{"TMUX names no socket", ","},
		{"relative socket", "relative/sock,91,0"},
		{"unclean socket", "/tmp/../tmp/sock,91,0"},
		{"over-long socket", "/tmp/" + strings.Repeat("a", tmux.MaxSocketPathLen) + ",91,0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(&exec.FakeSensitiveRunner{}, testTmux,
				func(k string) string { return map[string]string{"TMUX": tt.tmuxEnv}[k] },
				func() int { return 501 })
			if !errors.Is(err, ErrResolveSocket) {
				t.Fatalf("New = %v, want ErrResolveSocket", err)
			}
		})
	}

	// A relative tmux path is refused here rather than at the runner, so the
	// binary is chosen by this code and never by a PATH lookup against the live
	// process environment.
	if _, err := New(&exec.FakeSensitiveRunner{}, "tmux", func(string) string { return "" }, func() int { return 501 }); !errors.Is(err, ErrResolveSocket) {
		t.Errorf("New(relative tmux) = %v, want ErrResolveSocket", err)
	}
}

// TestEveryCommandIsPinnedToTheSocket is the structural assertion: an unpinned
// command reaches whichever server the environment happens to select, which for
// a close is somebody else's session.
//
// It drives Start, Close, and Probe far enough that each issues its commands,
// then asserts over the RECORDED calls rather than per-method, so a future
// command added without going through pinned() shows up here.
func TestEveryCommandIsPinnedToTheSocket(t *testing.T) {
	spec, tag := newSpec(t)
	name := spec.OwnershipName()
	live := row("91", "1700000000", "$1", name)

	run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
		exec.KindTmuxCreate:    func() (exec.SensitiveResult, error) { return stdout(live), nil },
		exec.KindTmuxProbe:     func() (exec.SensitiveResult, error) { return stdout(live), nil },
		exec.KindTmuxReconcile: func() (exec.SensitiveResult, error) { return stdout(live), nil },
	}}.runFunc}

	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))
	ctx := context.Background()

	res := a.Start(ctx, spec)
	if res.Outcome() != backend.RefKnown {
		t.Fatalf("Start outcome = %v, want ref-known (%v)", res.Outcome(), res)
	}
	ref, ok := res.Ref()
	if !ok {
		t.Fatal("a ref-known result carried no reference")
	}
	_ = a.Probe(ctx, ref)
	_ = a.Close(ctx, ref)

	calls := run.Calls()
	if len(calls) == 0 {
		t.Fatal("no commands recorded — the test drove nothing")
	}
	wantPin := []exec.Arg{exec.MustFixed("-S"), exec.Opaque(testSocket)}
	seen := map[exec.CommandKind]bool{}
	for i, cmd := range calls {
		if len(cmd.Args) < 2 || !cmd.Args[0].Equal(wantPin[0]) || !cmd.Args[1].Equal(wantPin[1]) {
			t.Errorf("call %d (kind %v) does not lead with the socket pin", i, cmd.Kind)
		}
		if !cmd.Path.Equal(exec.Secret(testTmux)) {
			t.Errorf("call %d ran a binary other than the resolved tmux", i)
		}
		seen[cmd.Kind] = true
	}
	// Each operation must reach the runner under its OWN kind. A kind that
	// never appears means the method bailed before issuing its command, and a
	// test that drove it would be asserting nothing about it.
	for _, kind := range []exec.CommandKind{
		exec.KindTmuxReadiness, exec.KindTmuxCreate, exec.KindTmuxProbe, exec.KindTmuxCleanup,
	} {
		if !seen[kind] {
			t.Errorf("no command recorded for kind %v", kind)
		}
	}
	_ = tag
}

// TestStartReturnsRefKnownOnACleanCreate also pins that the reference carries
// the spec's tag: Ref.Validate re-parses the session name and requires it to
// carry that exact tag, so a reference built from a reply about a different
// session cannot validate.
func TestStartReturnsRefKnownOnACleanCreate(t *testing.T) {
	spec, tag := newSpec(t)
	name := spec.OwnershipName()

	run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
		exec.KindTmuxCreate: func() (exec.SensitiveResult, error) {
			return stdout(row("91", "1700000000", "$1", name)), nil
		},
	}}.runFunc}

	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))
	res := a.Start(context.Background(), spec)

	if err := res.Validate(); err != nil {
		t.Fatalf("result does not validate: %v", err)
	}
	if res.Outcome() != backend.RefKnown {
		t.Fatalf("outcome = %v, want ref-known", res.Outcome())
	}
	if res.Failed() {
		t.Error("a clean create reported a failure")
	}
	ref, _ := res.Ref()
	if ref.Tag() != tag {
		t.Errorf("reference tag = %v, want the spec's %v", ref.Tag(), tag)
	}
	if ref.Source() != backend.TmuxDefaultServer() {
		t.Errorf("reference source = %v, want tmux-default", ref.Source())
	}
	if ref.OwnershipName() != name {
		t.Errorf("ownership name = %q, want %q", ref.OwnershipName(), name)
	}
}

// TestStartClassifiesTheThreeOutcomes is the matrix. Each row is a way a create
// can end, and the outcome column is what the service acts on: NotMutated means
// no cleanup, RefKnown means close exactly this reference, OutcomeUnknown means
// there is nothing we are entitled to close.
func TestStartClassifiesTheThreeOutcomes(t *testing.T) {
	spec, _ := newSpec(t)
	name := spec.OwnershipName()
	mine := row("91", "1700000000", "$1", name)
	stranger := row("91", "1700000000", "$2", "someone-elses-session")

	tests := []struct {
		name      string
		create    func() (exec.SensitiveResult, error)
		reconcile func() (exec.SensitiveResult, error)
		want      backend.MutationOutcome
		wantClass backend.StartFailureClass
	}{
		{
			name: "duplicate name is a definitive pre-mutation refusal",
			create: func() (exec.SensitiveResult, error) {
				return stderr("duplicate session: " + name), errors.New("exit 1")
			},
			want:      backend.NotMutated,
			wantClass: backend.FailureNameCollision,
		},
		{
			// The create failed and the server has no session by our name:
			// confirmed absent.
			name:      "create failed and reconcile finds nothing",
			create:    func() (exec.SensitiveResult, error) { return stderr("some other failure"), errors.New("exit 1") },
			reconcile: func() (exec.SensitiveResult, error) { return stdout(stranger), nil },
			want:      backend.NotMutated,
			wantClass: backend.FailureUnavailable,
		},
		{
			// The create failed but the session is there — created, then the
			// reply was lost. The service must close it.
			name:      "create failed and reconcile finds our session",
			create:    func() (exec.SensitiveResult, error) { return stderr("some other failure"), errors.New("exit 1") },
			reconcile: func() (exec.SensitiveResult, error) { return stdout(mine + "\n" + stranger), nil },
			want:      backend.RefKnown,
			wantClass: backend.FailureUnavailable,
		},
		{
			// No server at all proves absence — a server that does not exist is
			// not hiding our session.
			name:   "create failed and no server is running",
			create: func() (exec.SensitiveResult, error) { return stderr("boom"), errors.New("exit 1") },
			reconcile: func() (exec.SensitiveResult, error) {
				return stderr("no server running on /tmp/x"), errors.New("exit 1")
			},
			want:      backend.NotMutated,
			wantClass: backend.FailureUnavailable,
		},
		{
			// The listing could not be read, so absence is unproven. This is
			// the honest end state, not a failure to try harder.
			name:      "create failed and reconcile cannot read the server",
			create:    func() (exec.SensitiveResult, error) { return stderr("boom"), errors.New("exit 1") },
			reconcile: func() (exec.SensitiveResult, error) { return stderr("permission denied"), errors.New("exit 1") },
			want:      backend.OutcomeUnknown,
			wantClass: backend.FailureUnavailable,
		},
		{
			// A truncated listing cannot prove absence: the row we want may be
			// in the part we did not get.
			name:   "create failed and the listing was truncated",
			create: func() (exec.SensitiveResult, error) { return stderr("boom"), errors.New("exit 1") },
			reconcile: func() (exec.SensitiveResult, error) {
				return exec.SensitiveResult{
					Stdout: exec.BoundedOutputForTest([]byte(stranger), exec.OutputOverflowed),
					Stderr: exec.BoundedOutputForTest(nil, exec.OutputComplete),
				}, nil
			},
			want:      backend.OutcomeUnknown,
			wantClass: backend.FailureUnavailable,
		},
		{
			// Exit zero with nothing parseable is the same ambiguity as a
			// failure — reconcile rather than assume success or absence.
			name:      "create succeeded but said nothing parseable",
			create:    func() (exec.SensitiveResult, error) { return stdout("not a row"), nil },
			reconcile: func() (exec.SensitiveResult, error) { return stdout(stranger), nil },
			want:      backend.NotMutated,
			wantClass: backend.FailureMalformedResponse,
		},
		{
			// A reply describing a DIFFERENT session must never become our
			// reference.
			name:      "create reported a session that is not ours",
			create:    func() (exec.SensitiveResult, error) { return stdout(stranger), nil },
			reconcile: func() (exec.SensitiveResult, error) { return stdout(stranger), nil },
			want:      backend.NotMutated,
			wantClass: backend.FailureMalformedResponse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			byKind := map[exec.CommandKind]func() (exec.SensitiveResult, error){exec.KindTmuxCreate: tt.create}
			if tt.reconcile != nil {
				byKind[exec.KindTmuxReconcile] = tt.reconcile
			}
			run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: byKind}.runFunc}
			a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))

			res := a.Start(context.Background(), spec)
			if err := res.Validate(); err != nil {
				t.Fatalf("result does not validate: %v — an invalid result makes the service treat it as unknown", err)
			}
			if res.Outcome() != tt.want {
				t.Errorf("outcome = %v, want %v", res.Outcome(), tt.want)
			}
			cause, ok := res.Cause()
			if !ok {
				t.Fatal("no cause recorded")
			}
			if cause.Class() != tt.wantClass {
				t.Errorf("cause class = %v, want %v", cause.Class(), tt.wantClass)
			}
		})
	}
}

// TestReconcileRunsExactlyOnce: each extra attempt is another chance to match a
// session that appeared for some other reason, so the bound is part of the
// contract rather than an implementation detail.
func TestReconcileRunsExactlyOnce(t *testing.T) {
	spec, _ := newSpec(t)
	reconciles := 0
	run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
		exec.KindTmuxCreate: func() (exec.SensitiveResult, error) { return stderr("boom"), errors.New("exit 1") },
		exec.KindTmuxReconcile: func() (exec.SensitiveResult, error) {
			reconciles++
			return stderr("permission denied"), errors.New("exit 1")
		},
	}}.runFunc}

	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))
	if res := a.Start(context.Background(), spec); res.Outcome() != backend.OutcomeUnknown {
		t.Fatalf("outcome = %v, want outcome-unknown", res.Outcome())
	}
	if reconciles != 1 {
		t.Errorf("reconciled %d times, want exactly 1", reconciles)
	}
}

// TestStartRefusesAnOldTmuxBeforeCreating: below 2.2 tmux echoes #{pid} back
// literally. Catching that at readiness means the operator is told their tmux is
// too old, rather than being sent to look for a protocol bug — and it must
// happen BEFORE the create, so the refusal is honestly NotMutated.
func TestStartRefusesAnOldTmuxBeforeCreating(t *testing.T) {
	spec, _ := newSpec(t)
	run := &exec.FakeSensitiveRunner{RunFunc: scripted{version: "tmux 2.1"}.runFunc}
	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))

	res := a.Start(context.Background(), spec)
	if res.Outcome() != backend.NotMutated {
		t.Fatalf("outcome = %v, want not-mutated", res.Outcome())
	}
	cause, _ := res.Cause()
	if cause.Class() != backend.FailureIncompatible {
		t.Errorf("cause = %v, want backend-incompatible", cause.Class())
	}
	for _, cmd := range run.Calls() {
		if cmd.Kind == exec.KindTmuxCreate {
			t.Fatal("a create ran despite the version refusal — the result claims nothing was mutated")
		}
	}
}

// TestCloseAndProbeRefuseARestartedServer is the guarantee native ids exist to
// provide: a restarted tmux hands $1 to a completely different session, so a
// close that skipped the incarnation check would kill a stranger's window.
func TestCloseAndProbeRefuseARestartedServer(t *testing.T) {
	spec, _ := newSpec(t)
	name := spec.OwnershipName()

	run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
		exec.KindTmuxCreate: func() (exec.SensitiveResult, error) {
			return stdout(row("91", "1700000000", "$1", name)), nil
		},
		// Same socket, same name, DIFFERENT pid and start time: a restart.
		exec.KindTmuxProbe: func() (exec.SensitiveResult, error) {
			return stdout(row("777", "1700009999", "$1", name)), nil
		},
	}}.runFunc}

	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))
	res := a.Start(context.Background(), spec)
	ref, ok := res.Ref()
	if !ok {
		t.Fatalf("no reference from Start: %v", res)
	}

	closed := a.Close(context.Background(), ref)
	if closed.State() != backend.CloseIdentityMismatch {
		t.Errorf("Close = %v, want identity-mismatch", closed.State())
	}
	if closed.State().SatisfiesRollback() {
		t.Error("a refused close reported the rollback as satisfied")
	}
	for _, cmd := range run.Calls() {
		if cmd.Kind == exec.KindTmuxCleanup {
			t.Fatal("a kill ran against a restarted server")
		}
	}

	probed := a.Probe(context.Background(), ref)
	if probed.State() != backend.ProbeIdentityMismatch {
		t.Errorf("Probe = %v, want identity-mismatch", probed.State())
	}
	if probed.State().Conclusive() {
		t.Error("a mismatched probe reported a conclusive answer")
	}
}

// TestCloseKillsByNativeIDNeverByName is forgectl#237 in one assertion: a name
// handed to `-t` goes through tmux's target grammar, where a missing session
// falls through to a PREFIX sibling.
func TestCloseKillsByNativeIDNeverByName(t *testing.T) {
	spec, _ := newSpec(t)
	name := spec.OwnershipName()
	live := row("91", "1700000000", "$7", name)

	run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
		exec.KindTmuxCreate: func() (exec.SensitiveResult, error) { return stdout(live), nil },
		exec.KindTmuxProbe:  func() (exec.SensitiveResult, error) { return stdout(live), nil },
	}}.runFunc}

	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))
	ref, ok := a.Start(context.Background(), spec).Ref()
	if !ok {
		t.Fatal("no reference from Start")
	}
	if got := a.Close(context.Background(), ref); got.State() != backend.CloseClosed {
		t.Fatalf("Close = %v, want closed", got.State())
	}

	var kill exec.SensitiveCommand
	for _, cmd := range run.Calls() {
		if cmd.Kind == exec.KindTmuxCleanup {
			kill = cmd
		}
	}
	if kill.Kind != exec.KindTmuxCleanup {
		t.Fatal("no cleanup command was recorded")
	}
	// The operand is the native id from the listing, and never the name.
	target := kill.Args[len(kill.Args)-1]
	if !target.Equal(exec.Opaque("$7")) {
		t.Error("kill-session did not target the native id from the listing")
	}
	if target.Equal(exec.Opaque(name)) {
		t.Error("kill-session targeted the session NAME — that is forgectl#237")
	}
}

// TestCloseReportsAlreadyGoneRatherThanFailing: the rollback obligation is that
// the object be absent, and it is. Reporting a failure would leave the service
// believing something was left behind.
func TestCloseReportsAlreadyGoneRatherThanFailing(t *testing.T) {
	spec, _ := newSpec(t)
	name := spec.OwnershipName()

	for _, tt := range []struct {
		name  string
		probe func() (exec.SensitiveResult, error)
	}{
		{"no session by our name", func() (exec.SensitiveResult, error) {
			return stdout(row("91", "1700000000", "$2", "someone-else")), nil
		}},
		{"no server at all", func() (exec.SensitiveResult, error) {
			return stderr("no server running on /tmp/x"), errors.New("exit 1")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
				exec.KindTmuxCreate: func() (exec.SensitiveResult, error) {
					return stdout(row("91", "1700000000", "$1", name)), nil
				},
				exec.KindTmuxProbe: tt.probe,
			}}.runFunc}

			a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))
			ref, ok := a.Start(context.Background(), spec).Ref()
			if !ok {
				t.Fatal("no reference from Start")
			}

			got := a.Close(context.Background(), ref)
			if got.State() != backend.CloseAlreadyGone {
				t.Errorf("Close = %v, want already-gone", got.State())
			}
			if !got.State().SatisfiesRollback() {
				t.Error("confirmed absence did not satisfy the rollback")
			}
			if err := got.Validate(); err != nil {
				t.Errorf("close result does not validate: %v", err)
			}
		})
	}
}

// TestCloseWillNotCallAbsenceOnATruncatedListing: reporting gone from a
// half-read reply discharges an obligation that is still outstanding.
func TestCloseWillNotCallAbsenceOnATruncatedListing(t *testing.T) {
	spec, _ := newSpec(t)
	name := spec.OwnershipName()

	run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
		exec.KindTmuxCreate: func() (exec.SensitiveResult, error) {
			return stdout(row("91", "1700000000", "$1", name)), nil
		},
		exec.KindTmuxProbe: func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{
				Stdout: exec.BoundedOutputForTest([]byte("partial"), exec.OutputOverflowed),
				Stderr: exec.BoundedOutputForTest(nil, exec.OutputComplete),
			}, nil
		},
	}}.runFunc}

	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))
	ref, ok := a.Start(context.Background(), spec).Ref()
	if !ok {
		t.Fatal("no reference from Start")
	}

	got := a.Close(context.Background(), ref)
	if got.State() == backend.CloseAlreadyGone {
		t.Fatal("a truncated listing was read as confirmed absence")
	}
	if got.State() != backend.CloseUnreadable {
		t.Errorf("Close = %v, want unreadable", got.State())
	}
	if got.State().SatisfiesRollback() {
		t.Error("an unreadable close reported the rollback as satisfied")
	}
}

// TestCloseRefusesAReferenceFromADifferentSelectionChain: two chains that
// resolve to the same path today are still different chains, and a reference
// taken against the inherited server must not be answered by a default-socket
// adapter.
func TestCloseRefusesAReferenceFromADifferentSelectionChain(t *testing.T) {
	spec, tag := newSpec(t)
	server, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: testSocket, Version: "tmux 3.7b", Inode: liveInode,
	})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	id, err := backend.NewTmuxIdentity(spec.OwnershipName())
	if err != nil {
		t.Fatalf("NewTmuxIdentity: %v", err)
	}
	// A reference bound to the INHERITED chain.
	ref, err := backend.NewTmuxRef(backend.TmuxCurrentServer(), server, tag, id)
	if err != nil {
		t.Fatalf("NewTmuxRef: %v", err)
	}

	run := &exec.FakeSensitiveRunner{}
	// …handed to an adapter on the DEFAULT chain.
	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))

	got := a.Close(context.Background(), ref)
	if got.State() != backend.CloseIdentityMismatch {
		t.Errorf("Close = %v, want identity-mismatch", got.State())
	}
	if len(run.Calls()) != 0 {
		t.Errorf("ran %d commands before refusing; the refusal must cost nothing", len(run.Calls()))
	}
}

// TestFingerprintFailsClosedWithoutAnInode: Fingerprint requires a non-zero
// inode because that is the field that turns over on a restart. A stat that
// yields no Stat_t must not produce a digest that would match across one.
func TestFingerprintFailsClosedWithoutAnInode(t *testing.T) {
	spec, _ := newSpec(t)
	name := spec.OwnershipName()
	live := row("91", "1700000000", "$1", name)

	run := &exec.FakeSensitiveRunner{RunFunc: scripted{byKind: map[exec.CommandKind]func() (exec.SensitiveResult, error){
		exec.KindTmuxCreate:    func() (exec.SensitiveResult, error) { return stdout(live), nil },
		exec.KindTmuxReconcile: func() (exec.SensitiveResult, error) { return stdout(live), nil },
	}}.runFunc}

	// Sys() returns nil, so fillStat leaves Inode zero.
	a := newTestAdapter(t, run, nil, WithLstat(func(string) (os.FileInfo, error) {
		return fakeInfo{sys: nil}, nil
	}))

	res := a.Start(context.Background(), spec)
	if res.Outcome() == backend.RefKnown {
		t.Fatal("a reference was built without a usable incarnation fingerprint")
	}
	if err := res.Validate(); err != nil {
		t.Errorf("result does not validate: %v", err)
	}
}

// TestStartRefusesAnInvalidSpec keeps a malformed spec from reaching tmux at
// all, so the refusal is honestly NotMutated.
func TestStartRefusesAnInvalidSpec(t *testing.T) {
	run := &exec.FakeSensitiveRunner{}
	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode)))

	res := a.Start(context.Background(), backend.StartSpec{})
	if res.Outcome() != backend.NotMutated {
		t.Errorf("outcome = %v, want not-mutated", res.Outcome())
	}
	if len(run.Calls()) != 0 {
		t.Errorf("ran %d commands for an invalid spec", len(run.Calls()))
	}
}

func TestAdapterSatisfiesCapabilities(t *testing.T) {
	a := newTestAdapter(t, &exec.FakeSensitiveRunner{}, nil)
	if a.Kind() != backend.KindTmux {
		t.Errorf("Kind() = %v, want tmux", a.Kind())
	}
	// RequireCapabilities is the gate the launch runs BEFORE binding a listener
	// or mutating a manager, precisely so it never discovers at rollback time
	// that the adapter cannot close what it created.
	if _, err := backend.RequireCapabilities(a); err != nil {
		t.Errorf("RequireCapabilities: %v", err)
	}
}

func TestParseVersion(t *testing.T) {
	for _, tt := range []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"tmux 3.7b", 3, 7, true},
		{"tmux 2.2", 2, 2, true},
		{"tmux next-3.4", 3, 4, true},
		{"tmux 10.12", 10, 12, true},
		{"tmux", 0, 0, false},
		{"", 0, 0, false},
		{"tmux 3", 0, 0, false},
		{"tmux master", 0, 0, false},
	} {
		t.Run(tt.in, func(t *testing.T) {
			major, minor, ok := parseVersion(tt.in)
			if ok != tt.ok || major != tt.major || minor != tt.minor {
				t.Errorf("parseVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tt.in, major, minor, ok, tt.major, tt.minor, tt.ok)
			}
		})
	}
}

// TestParseRowsRequiresAnExactFieldCount: a session NAME may legally contain
// the field separator, and under a `len(f) < N` check such a row would parse
// with every later field shifted — offering a well-formed-looking native id
// that names a different session.
func TestParseRowsRequiresAnExactFieldCount(t *testing.T) {
	forged := row("91", "1700000000", "$1", "name"+tmux.FieldSep+"extra")
	rows, complete := parseRows(exec.BoundedOutputForTest([]byte(forged), exec.OutputComplete))
	if !complete {
		t.Fatal("the fixture was reported incomplete")
	}
	if len(rows) != 0 {
		t.Errorf("a row with a separator in its NAME was parsed: %+v", rows)
	}

	// And a well-formed row still parses, so the check is not simply refusing
	// everything.
	good := row("91", "1700000000", "$1", "fc-surface-x")
	rows, _ = parseRows(exec.BoundedOutputForTest([]byte(good), exec.OutputComplete))
	if len(rows) != 1 || rows[0].id != "$1" {
		t.Errorf("a well-formed row did not parse: %+v", rows)
	}
}

// TestSoleRowRejectsAReplyAboutAnotherSession: `new-session -P` describes the
// session it just made, so a reply naming something else is a reply about a
// different object and must never become a reference we would later close.
func TestSoleRowRejectsAReplyAboutAnotherSession(t *testing.T) {
	mine := row("91", "1700000000", "$1", "fc-surface-mine")
	theirs := row("91", "1700000000", "$2", "fc-surface-theirs")

	if _, ok := soleRow(exec.BoundedOutputForTest([]byte(theirs), exec.OutputComplete), "fc-surface-mine"); ok {
		t.Error("a reply about another session was accepted")
	}
	if _, ok := soleRow(exec.BoundedOutputForTest([]byte(mine+"\n"+theirs), exec.OutputComplete), "fc-surface-mine"); ok {
		t.Error("a multi-row create reply was accepted")
	}
	if _, ok := soleRow(exec.BoundedOutputForTest([]byte(mine), exec.OutputOverflowed), "fc-surface-mine"); ok {
		t.Error("a truncated create reply was accepted")
	}
	if _, ok := soleRow(exec.BoundedOutputForTest([]byte(mine), exec.OutputComplete), "fc-surface-mine"); !ok {
		t.Error("the matching single row was rejected")
	}
}

// TestStderrMatchingIsExactEquality: under Contains, a session named after the
// diagnostic would forge the duplicate-name verdict — the one class the
// contract lets a caller retry.
func TestStderrMatchingIsExactEquality(t *testing.T) {
	want := "duplicate session: fc-surface-abc"
	if !stderrEquals(exec.BoundedOutputForTest([]byte(want+"\n"), exec.OutputComplete), want) {
		t.Error("the exact diagnostic did not match")
	}
	if stderrEquals(exec.BoundedOutputForTest([]byte("note: "+want), exec.OutputComplete), want) {
		t.Error("a diagnostic merely CONTAINING the expected text matched")
	}
	if stderrEquals(exec.BoundedOutputForTest([]byte(want), exec.OutputOverflowed), want) {
		t.Error("a truncated stderr matched — a prefix is not equality")
	}
}
