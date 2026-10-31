package hcadapter

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// fakeHC is a scripted hc: one terminal map keyed by address, and a core id.
type fakeHC struct {
	coreID    string
	terminals map[string]string // address -> `hc get --json` reply
	// createReply, when set, is what `hc run` prints; createErr is its stderr
	// and createExit its exit code (0 for success).
	createReply string
	createErr   string
	createExit  int
	// sendKind, when set, is the error kind `hc send` fails with.
	sendKind string
	// partialRead makes `hc read` report an incomplete screen.
	partialRead bool
	// lsReply, when set, is what `hc ls` prints; lsFail makes it fail.
	lsReply string
	lsFail  bool
	verbs   []string
}

func (f *fakeHC) runner() *exec.FakeSensitiveRunner {
	return &exec.FakeSensitiveRunner{RunFunc: func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		verb := verbOf(cmd)
		f.verbs = append(f.verbs, verb)
		switch verb {
		case "server":
			return ok(`{"core":{"id":"` + f.coreID + `","name":"m"},"version":"0.1.15"}`), nil
		case "run":
			if f.createExit != 0 {
				return fail(cmd.Kind, f.createErr, f.createExit)
			}
			return ok(f.createReply), nil
		case "get":
			addr := lastAddress(cmd, f.terminals)
			if reply, found := f.terminals[addr]; found {
				return ok(reply), nil
			}
			return fail(cmd.Kind, `{"error":{"kind":"not_found","message":"no terminal"}}`, 3)
		case "ls":
			if f.lsFail {
				return fail(cmd.Kind, `{"error":{"kind":"internal","message":"?"}}`, 1)
			}
			if f.lsReply != "" {
				return ok(f.lsReply), nil
			}
			return ok(`{"hidden_finished":0,"terminals":[]}`), nil
		case "read":
			if f.partialRead {
				return ok(`{"complete":false,"how":"screen","text":"hel"}`), nil
			}
			return ok(`{"complete":true,"how":"screen","text":"hello\nREPORT m1: done\n"}`), nil
		case "send":
			if f.sendKind != "" {
				// Exit 1 for every kind: the adapter must map on kind.
				return fail(cmd.Kind, `{"error":{"kind":"`+f.sendKind+`","message":"no"}}`, 1)
			}
			return ok(`{"started":true,"status":"working","turn":2}`), nil
		case "kill", "rm", "keys":
			return ok(`{}`), nil
		}
		return exec.SensitiveResult{}, errors.New("unscripted verb " + verb)
	}}
}

// verbArgs are the first arguments the adapter issues, as the seam spells
// them; MustFixed takes only a constant, so each is written out.
var verbArgs = map[string]exec.Arg{
	"server": exec.MustFixed("server"), "run": exec.MustFixed("run"), "get": exec.MustFixed("get"),
	"ls": exec.MustFixed("ls"), "read": exec.MustFixed("read"), "send": exec.MustFixed("send"),
	"kill": exec.MustFixed("kill"), "rm": exec.MustFixed("rm"), "keys": exec.MustFixed("keys"),
}

func verbOf(cmd exec.SensitiveCommand) string {
	for v, arg := range verbArgs {
		if len(cmd.Args) > 0 && cmd.Args[0].Equal(arg) {
			return v
		}
	}
	return "?"
}

// lastAddress finds which scripted address the command's last argument is.
func lastAddress(cmd exec.SensitiveCommand, terminals map[string]string) string {
	last := cmd.Args[len(cmd.Args)-1]
	for addr := range terminals {
		if last.Equal(exec.Opaque(addr)) {
			return addr
		}
	}
	for _, addr := range []string{"t1", "t2", "t9"} {
		if last.Equal(exec.Opaque(addr)) {
			return addr
		}
	}
	return ""
}

func ok(stdout string) exec.SensitiveResult {
	return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest([]byte(stdout), exec.OutputComplete),
		Stderr: exec.BoundedOutputForTest(nil, exec.OutputComplete)}
}

func fail(kind exec.CommandKind, stderr string, code int) (exec.SensitiveResult, error) {
	return exec.SensitiveResult{
		Stdout:   exec.BoundedOutputForTest(nil, exec.OutputComplete),
		Stderr:   exec.BoundedOutputForTest([]byte(stderr), exec.OutputComplete),
		ExitCode: code,
	}, exec.SensitiveErrorForTest(kind, exec.OutcomeExit)
}

func newTestAdapter(t *testing.T, f *fakeHC, session string) *Adapter {
	t.Helper()
	a, err := New(f.runner(), "/usr/local/bin/hc", func(k string) string {
		if k == "HC_SESSION" {
			return session
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testSpec(t *testing.T) backend.StartSpec {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	boot, err := backend.NewBootstrapCommand(exec.Opaque("'/bin/forgectl' __trampoline x"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := backend.NewStartSpec("/repo/.claude/worktrees/x", "x", tag, boot)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func getReply(addr, owner string, created int64, state, status string) string {
	return `{"address":"` + addr + `","created_ms":` + itoa(created) + `,"state":"` + state +
		`","labels":{"forgectl.surface":"` + owner + `"},"turn":1,"status":` + status + `}`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func startWorker(t *testing.T, f *fakeHC, a *Adapter) backend.Ref {
	t.Helper()
	spec := testSpec(t)
	f.createReply = `{"address":"t1","agent":"sh","cwd":"/repo","name":"x","state":"live","turn":0}`
	f.terminals["t1"] = getReply("t1", spec.OwnershipName(), 1793409108596, "live", `{"app":"claude-code","msg":"","state":"idle"}`)
	res := a.Start(context.Background(), spec)
	ref, has := res.Ref()
	if !has || res.Failed() {
		t.Fatalf("Start = %v, want a clean RefKnown", res)
	}
	return ref
}

func TestStartRefRoundTrips(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	data, err := ref.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := backend.DecodeRef(data)
	if err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	id, err := back.HcIdentity()
	if err != nil || id.Terminal() != "t1" || id.CreatedMS() != 1793409108596 {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if a.Session() != "hc:default" {
		t.Errorf("Session = %q, want hc:default", a.Session())
	}
}

func TestStartNameTakenIsNotMutated(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{},
		createExit: 1, createErr: `{"error":{"kind":"name_taken","message":"taken"}}`}
	a := newTestAdapter(t, f, "")
	res := a.Start(context.Background(), testSpec(t))
	cause, _ := res.Cause()
	if res.Outcome() != backend.NotMutated || cause.Class() != backend.FailureNameCollision {
		t.Fatalf("Start = %v cause %v, want not-mutated name-collision", res, cause)
	}
}

func TestStartAmbiguousFailureReconciles(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{},
		createExit: 1, createErr: `{"error":{"kind":"internal","message":"?"}}`}
	a := newTestAdapter(t, f, "")
	res := a.Start(context.Background(), testSpec(t))
	if res.Outcome() != backend.NotMutated {
		t.Fatalf("Start = %v, want not-mutated after an empty reconcile listing", res)
	}
	if f.verbs[len(f.verbs)-1] != "ls" {
		t.Errorf("verbs = %v, want a reconciling ls last", f.verbs)
	}
}

func TestCloseKillsThenRemoves(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	f.verbs = nil
	if res := a.Close(context.Background(), ref); res.State() != backend.CloseClosed {
		t.Fatalf("Close = %v, want closed", res)
	}
	want := []string{"server", "get", "kill", "rm"}
	if len(f.verbs) != len(want) {
		t.Fatalf("verbs = %v, want %v", f.verbs, want)
	}
	for i := range want {
		if f.verbs[i] != want[i] {
			t.Fatalf("verbs = %v, want %v", f.verbs, want)
		}
	}
}

func TestCloseRefusesReusedAddress(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	// Same address, same label, a different creation time: hc reused t1.
	f.terminals["t1"] = getReply("t1", ref.OwnershipName(), 1793409999999, "live", `null`)
	f.verbs = nil
	if res := a.Close(context.Background(), ref); res.State() != backend.CloseIdentityMismatch {
		t.Fatalf("Close = %v, want identity-mismatch", res)
	}
	for _, v := range f.verbs {
		if v == "kill" || v == "rm" {
			t.Fatalf("verbs = %v: a mismatched terminal was acted on", f.verbs)
		}
	}
}

func TestCloseRefusesForeignLabel(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	f.terminals["t1"] = getReply("t1", "fc-surface-someoneelse", 1793409108596, "live", `null`)
	if res := a.Close(context.Background(), ref); res.State() != backend.CloseIdentityMismatch {
		t.Fatalf("Close = %v, want identity-mismatch", res)
	}
}

func TestChangedCoreIsMismatch(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	f.coreID = "core2"
	if res := a.Probe(context.Background(), ref); res.State() != backend.ProbeIdentityMismatch {
		t.Fatalf("Probe = %v, want identity-mismatch", res)
	}
}

func TestOtherSessionIsMismatch(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	ref := startWorker(t, f, newTestAdapter(t, f, ""))
	if res := newTestAdapter(t, f, "scratch").Probe(context.Background(), ref); res.State() != backend.ProbeIdentityMismatch {
		t.Fatalf("Probe from another session = %v, want identity-mismatch", res)
	}
}

func TestProbeStates(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	if res := a.Probe(context.Background(), ref); res.State() != backend.ProbePresent {
		t.Fatalf("live Probe = %v", res)
	}
	f.terminals["t1"] = getReply("t1", ref.OwnershipName(), 1793409108596, "finished", `null`)
	if res := a.Probe(context.Background(), ref); res.State() != backend.ProbeGone {
		t.Fatalf("finished Probe = %v, want gone", res)
	}
	delete(f.terminals, "t1")
	if res := a.Probe(context.Background(), ref); res.State() != backend.ProbeGone {
		t.Fatalf("absent Probe = %v, want gone", res)
	}
	if res := a.Close(context.Background(), ref); res.State() != backend.CloseAlreadyGone {
		t.Fatalf("absent Close = %v, want already-gone", res)
	}
}

func TestWorkerScreenAndSend(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	w, err := a.WorkerScreen(context.Background(), ref)
	if err != nil || w.Status != StatusIdle || w.App != "claude-code" || w.Text != "hello\nREPORT m1: done" {
		t.Fatalf("WorkerScreen = %+v, %v", w, err)
	}
	sent, err := a.SendBrief(context.Background(), ref, "next step", false)
	if err != nil || sent.Status != StatusWorking || sent.Turn != 2 {
		t.Fatalf("SendBrief = %+v, %v", sent, err)
	}
	for _, kind := range []string{kindBlocked, kindStatusMismatch, kindFinished} {
		f.sendKind = kind
		if _, err := a.SendBrief(context.Background(), ref, "next step", false); !errors.Is(err, ErrSendRefused) {
			t.Fatalf("%s SendBrief err = %v, want ErrSendRefused", kind, err)
		}
	}
	f.sendKind = "internal"
	if _, err := a.SendBrief(context.Background(), ref, "next step", false); !errors.Is(err, ErrSendFailed) {
		t.Fatalf("failed SendBrief err = %v, want ErrSendFailed", err)
	}
	delete(f.terminals, "t1")
	if _, err := a.WorkerScreen(context.Background(), ref); !errors.Is(err, ErrWorkerGone) {
		t.Fatalf("gone WorkerScreen err = %v", err)
	}
	// A send to a gone worker typed nothing: refused, never unknown.
	f.sendKind = ""
	if _, err := a.SendBrief(context.Background(), ref, "next step", false); !errors.Is(err, ErrSendRefused) {
		t.Fatalf("gone SendBrief err = %v, want ErrSendRefused", err)
	}
}

func TestSendBriefIfIdle(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	run := f.runner()
	a, err := New(run, "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ref := startWorker(t, f, a)
	for _, ifIdle := range []bool{true, false} {
		if _, err := a.SendBrief(context.Background(), ref, "go", ifIdle); err != nil {
			t.Fatal(err)
		}
		last, _ := run.Last()
		has := false
		for i := 0; i+1 < len(last.Args); i++ {
			if last.Args[i].Equal(exec.MustFixed("--if")) && last.Args[i+1].Equal(exec.MustFixed("idle")) {
				has = true
			}
		}
		if has != ifIdle {
			t.Errorf("ifIdle=%v: --if idle present = %v", ifIdle, has)
		}
	}
}

func TestIncompleteReadIsUnreadable(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}, partialRead: true}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	if _, err := a.WorkerScreen(context.Background(), ref); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
}

func TestCheckName(t *testing.T) {
	for name, want := range map[string]bool{
		"fix-login": true, "test": true, "tx1": true, "a.b": true, "9a": true,
		"": false, "T7": false, "t7": false, "t1r": false, "t12": false, "_x": false,
		"has space": false, "abcdefghijabcdefghijabcdefghij123": false,
	} {
		if got := CheckName(name) == nil; got != want {
			t.Errorf("CheckName(%q) ok = %v, want %v", name, got, want)
		}
	}
}

func TestNewRefusesOddSession(t *testing.T) {
	_, err := New(&exec.FakeSensitiveRunner{}, "/hc", func(string) string { return "a b" })
	if !errors.Is(err, ErrResolveSession) {
		t.Fatalf("err = %v", err)
	}
}

// TestReconcileBranches covers the labeled listing that settles a create
// whose outcome hc did not report.
func TestReconcileBranches(t *testing.T) {
	const created = 1793409108596
	one := `{"hidden_finished":0,"terminals":[{"address":"t1","state":"live"}]}`
	two := `{"hidden_finished":0,"terminals":[{"address":"t1","state":"live"},{"address":"t2","state":"live"}]}`
	cases := []struct {
		name    string
		ls      string
		lsFail  bool
		label   string // the label `hc get t1` reports; "" means the launch's own marker
		outcome backend.MutationOutcome
	}{
		{name: "one labeled terminal is a known ref the service closes", ls: one, outcome: backend.RefKnown},
		{name: "two labeled terminals are unknown", ls: two, outcome: backend.OutcomeUnknown},
		{name: "a terminal whose get shows another label is unknown", ls: one, label: "fc-surface-someoneelse", outcome: backend.OutcomeUnknown},
		{name: "a failed listing is unknown", lsFail: true, outcome: backend.OutcomeUnknown},
		{name: "an empty listing created nothing", outcome: backend.NotMutated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec(t)
			label := tc.label
			if label == "" {
				label = spec.OwnershipName()
			}
			f := &fakeHC{coreID: "core1", terminals: map[string]string{
				"t1": getReply("t1", label, created, "live", `null`),
			}, createExit: 1, createErr: `{"error":{"kind":"internal","message":"?"}}`, lsReply: tc.ls, lsFail: tc.lsFail}
			run := f.runner()
			a, err := New(run, "/usr/local/bin/hc", func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			res := a.Start(context.Background(), spec)
			if res.Outcome() != tc.outcome {
				t.Fatalf("outcome = %v, want %v", res.Outcome(), tc.outcome)
			}
			if tc.outcome == backend.RefKnown {
				if !res.Failed() {
					t.Error("a reconciled ref must carry the create's cause, so the service closes it")
				}
				ref, _ := res.Ref()
				id, err := ref.HcIdentity()
				if err != nil || id.Terminal() != "t1" || id.CreatedMS() != created {
					t.Errorf("ref identity = %+v, %v", id, err)
				}
			}
			// The listing filters on exactly this launch's marker.
			want := exec.Opaque("forgectl.surface=" + spec.OwnershipName())
			found := false
			for _, c := range run.Calls() {
				if c.Kind == exec.KindHcReconcile && c.Args[len(c.Args)-1].Equal(want) {
					found = true
				}
			}
			if !found {
				t.Error("no reconcile listing filtered on the launch's ownership label")
			}
		})
	}
}

// TestDefaultSessionSpellingsMatch: HC_SESSION unset and HC_SESSION=default
// select hc's one default core, so a worker launched under one spelling is
// probed and closed under the other.
func TestDefaultSessionSpellingsMatch(t *testing.T) {
	for _, pair := range [][2]string{{"", "default"}, {"default", ""}} {
		f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
		ref := startWorker(t, f, newTestAdapter(t, f, pair[0]))
		other := newTestAdapter(t, f, pair[1])
		if res := other.Probe(context.Background(), ref); res.State() != backend.ProbePresent {
			t.Fatalf("launched with HC_SESSION=%q, probed with %q: %v", pair[0], pair[1], res)
		}
		if other.Session() != "hc:default" {
			t.Errorf("Session = %q", other.Session())
		}
	}
	// A named session is still its own selection.
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	ref := startWorker(t, f, newTestAdapter(t, f, ""))
	if res := newTestAdapter(t, f, "work").Probe(context.Background(), ref); res.State() != backend.ProbeIdentityMismatch {
		t.Fatalf("probe from a named session = %v, want identity-mismatch", res)
	}
}

func TestPressEscapeSendsOneEsc(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	run := f.runner()
	a, err := New(run, "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ref := startWorker(t, f, a)
	checked := Worker{Live: true, Status: StatusIdle, Turn: 1}
	if err := a.PressEscape(context.Background(), ref, checked); err != nil {
		t.Fatal(err)
	}
	last, _ := run.Last()
	n := len(last.Args)
	if !last.Args[0].Equal(exec.MustFixed("keys")) || !last.Args[n-2].Equal(exec.Opaque("t1")) || !last.Args[n-1].Equal(exec.MustFixed("esc")) {
		t.Fatal("PressEscape did not run hc keys <t1> esc")
	}
	delete(f.terminals, "t1")
	if err := a.PressEscape(context.Background(), ref, checked); !errors.Is(err, ErrSendRefused) {
		t.Fatalf("Escape to a gone worker: %v, want ErrSendRefused", err)
	}
}

// TestPressEscapeRefusesAChangedWorker: Escape interrupts a running turn and
// hc keys has no --if idle, so a worker whose status or turn moved since the
// brief's checks gets no key at all.
func TestPressEscapeRefusesAChangedWorker(t *testing.T) {
	checked := Worker{Live: true, Status: StatusIdle, Turn: 1}
	for name, mutate := range map[string]func(string) string{
		"turn started": func(r string) string { return strings.Replace(r, `"state":"idle"`, `"state":"working"`, 1) },
		"blocked":      func(r string) string { return strings.Replace(r, `"state":"idle"`, `"state":"blocked"`, 1) },
		"turn moved":   func(r string) string { return strings.Replace(r, `"turn":1`, `"turn":2`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
			a := newTestAdapter(t, f, "")
			ref := startWorker(t, f, a)
			f.terminals["t1"] = mutate(f.terminals["t1"])
			f.verbs = nil
			if err := a.PressEscape(context.Background(), ref, checked); !errors.Is(err, ErrSendRefused) {
				t.Fatalf("PressEscape = %v, want ErrSendRefused", err)
			}
			if slices.Contains(f.verbs, "keys") {
				t.Fatalf("a key was sent to a changed worker: %v", f.verbs)
			}
		})
	}
}
