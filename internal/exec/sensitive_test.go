package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// dualHandler fans one record out to two handlers. It exists because the two
// slog handlers differ exactly where a redaction gap would live: the JSON
// handler marshals (and so skips unexported fields), while the text handler
// renders a non-TextMarshaler value with fmt.Sprintf("%+v", v). Production
// installs the text handler, so a JSON-only capture cannot see the leak this
// package is built to prevent.
type dualHandler struct{ a, b slog.Handler }

func (d dualHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return d.a.Enabled(ctx, l) || d.b.Enabled(ctx, l)
}

func (d dualHandler) Handle(ctx context.Context, r slog.Record) error {
	_ = d.a.Handle(ctx, r.Clone())
	return d.b.Handle(ctx, r)
}

func (d dualHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return dualHandler{a: d.a.WithAttrs(as), b: d.b.WithAttrs(as)}
}

func (d dualHandler) WithGroup(name string) slog.Handler {
	return dualHandler{a: d.a.WithGroup(name), b: d.b.WithGroup(name)}
}

// captureSlog installs a debug-level capture — under both handler shapes — as
// the default logger for the test's duration. Debug level matters: the leak
// this seam exists to close was a slog.Debug call, so a capture at Info would
// prove nothing.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	prev := slog.Default()
	slog.SetDefault(slog.New(dualHandler{
		a: slog.NewJSONHandler(&buf, opts),
		b: slog.NewTextHandler(&buf, opts),
	}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

const (
	pathSentinel  = "SENTINEL-PATH-a1b2c3"
	arg0Sentinel  = "SENTINEL-ARG0-d4e5f6"
	arg1Sentinel  = "SENTINEL-ARG1-g7h8i9"
	arg2Sentinel  = "SENTINEL-ARG2-j1k2l3"
	envSentinel   = "SENTINEL-ENV-m4n5o6"
	herdrSentinel = "SENTINEL-HERDR-p7q8r9"
)

func allSentinels() []string {
	return []string{pathSentinel, arg0Sentinel, arg1Sentinel, arg2Sentinel, envSentinel, herdrSentinel}
}

// TestSensitiveCommand_SentinelNeverRendersThroughAnyPath is the load-bearing
// test. Every value that goes into the command carries a unique sentinel; the
// command is run against a binary that cannot start; and the resulting error,
// result, command, and captured log are then rendered through every path a
// caller or a logger could plausibly take. No sentinel may appear in any of
// them.
func TestSensitiveCommand_SentinelNeverRendersThroughAnyPath(t *testing.T) {
	logs := captureSlog(t)

	cmd := SensitiveCommand{
		Kind: KindTmuxCreate,
		Path: Secret("/nonexistent/" + pathSentinel),
		Args: []Arg{
			Opaque(arg0Sentinel),
			Opaque(arg1Sentinel),
			Opaque(arg2Sentinel),
		},
		Env: []EnvMutation{
			ReplaceCmuxSocketPath(envSentinel),
			ReplaceHerdrConfigPath(herdrSentinel),
			UnsetTmux(),
		},
		StdoutCap: 1024,
		StderrCap: 1024,
	}

	runner := NewOSSensitiveRunner()
	res, err := runner.RunSensitive(context.Background(), cmd)
	if err == nil {
		t.Fatal("expected the nonexistent binary to fail to start")
	}
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("expected ErrStartFailed, got %v", err)
	}

	assertNoSentinel(t, "start-failure", renderEveryWay(t, cmd, res, err), logs.String())

	// The captured logging must be non-empty, or "no sentinel appeared" would
	// be satisfied by the runner having logged nothing at all.
	if logs.Len() == 0 {
		t.Fatal("runner produced no log output; the leak assertion would be vacuous")
	}
	if !strings.Contains(logs.String(), "tmux.create") {
		t.Errorf("expected the closed command kind in the logs, got: %s", logs.String())
	}
}

// TestSensitiveCommand_SentinelNeverRendersFromARunThatProduced covers what the
// start-failure case above cannot: the paths where the process actually ran and
// the runner logs a populated result. A start failure never reaches those
// slog.Error calls, so without this the coverage would be narrower than the
// test name above suggests.
func TestSensitiveCommand_SentinelNeverRendersFromARunThatProduced(t *testing.T) {
	logs := captureSlog(t)
	runner, self := helperRunner(t, "flood:400000", defaultRetireBound)

	cmd := SensitiveCommand{
		Kind:      KindCmuxSnapshot,
		Path:      Secret(self),
		Args:      []Arg{Opaque(arg0Sentinel), Opaque(arg1Sentinel)},
		Env:       []EnvMutation{ReplaceCmuxSocketPath(envSentinel)},
		StdoutCap: 4096,
		StderrCap: 4096,
	}

	res, err := runner.RunSensitive(context.Background(), cmd)
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrOutputLimit", err)
	}
	if res.Stdout.Len() == 0 {
		t.Fatal("no output captured; the rendering assertions would be vacuous")
	}

	// The captured stdout is the helper's filler bytes, never a sentinel — the
	// sentinels are in the path, argv, and environment only.
	assertNoSentinel(t, "overflow", renderEveryWay(t, cmd, res, err), logs.String())
	if !strings.Contains(logs.String(), "output_limit") {
		t.Errorf("expected the closed outcome in the logs, got: %s", logs.String())
	}
}

func renderEveryWay(t *testing.T, cmd SensitiveCommand, res SensitiveResult, err error) map[string]string {
	t.Helper()
	return map[string]string{
		"err.Error()":           err.Error(),
		"%v of err":             fmt.Sprintf("%v", err),
		"%+v of err":            fmt.Sprintf("%+v", err),
		"%#v of err":            fmt.Sprintf("%#v", err),
		"%q of err":             fmt.Sprintf("%q", err),
		"%s of err":             fmt.Sprintf("%s", err),
		"errors.Unwrap(err)":    fmt.Sprintf("%v", errors.Unwrap(err)),
		"%v of result":          fmt.Sprintf("%v", res),
		"%+v of result":         fmt.Sprintf("%+v", res),
		"%#v of result":         fmt.Sprintf("%#v", res),
		"%v of command":         fmt.Sprintf("%v", cmd),
		"%+v of command":        fmt.Sprintf("%+v", cmd),
		"%#v of command":        fmt.Sprintf("%#v", cmd),
		"%q of command":         fmt.Sprintf("%q", cmd),
		"%v of command args":    fmt.Sprintf("%v", cmd.Args),
		"%+v of command env":    fmt.Sprintf("%+v", cmd.Env),
		"%#v of command path":   fmt.Sprintf("%#v", cmd.Path),
		"embedded in a struct":  fmt.Sprintf("%+v", newContainmentProbe(cmd, res)),
		"embedded, %#v":         fmt.Sprintf("%#v", newContainmentProbe(cmd, res)),
		"embedded, %v":          fmt.Sprintf("%v", newContainmentProbe(cmd, res)),
		"json.Marshal(err)":     mustMarshal(t, err),
		"json.Marshal(result)":  mustMarshal(t, res),
		"json.Marshal(command)": mustMarshal(t, cmd),
		"json.Marshal(args)":    mustMarshal(t, cmd.Args),
		"json.Marshal(env)":     mustMarshal(t, cmd.Env),
		"slog of err":           logLine(t, "err", err),
		"slog of result":        logLine(t, "res", res),
		"slog of command":       logLine(t, "cmd", cmd),
		"slog of path":          logLine(t, "path", cmd.Path),
		"slog of args":          logLine(t, "args", cmd.Args),
		"slog of env":           logLine(t, "env", cmd.Env),
		"slog of the struct":    logLine(t, "probe", newContainmentProbe(cmd, res)),
	}
}

// containmentProbe is the shape an adapter will actually have: opaque values
// held in *unexported* fields. fmt consults a value's Formatter/Stringer only
// when reflect.Value.CanInterface() is true, which is false through an
// unexported field — so a string payload would be printed verbatim here even
// though every direct rendering of the same type is redacted. This probe is
// what pins the closure containment; without it the guarantee is assumed.
type containmentProbe struct {
	path   SecretArg
	args   []Arg
	env    []EnvMutation
	stdout BoundedOutput
	label  string
}

func newContainmentProbe(cmd SensitiveCommand, res SensitiveResult) containmentProbe {
	return containmentProbe{
		path: cmd.Path, args: cmd.Args, env: cmd.Env, stdout: res.Stdout, label: "adapter",
	}
}

func assertNoSentinel(t *testing.T, phase string, renderings map[string]string, logs string) {
	t.Helper()
	renderings["captured runner logging"] = logs
	for name, rendered := range renderings {
		for _, s := range allSentinels() {
			if strings.Contains(rendered, s) {
				t.Errorf("[%s] %s leaked sentinel %s: %s", phase, name, s, rendered)
			}
		}
	}
}

// TestSensitiveCommand_RevealReachesExecCmd is the mirror of the leak test.
// Without it, a seam that dropped every value on the floor would pass the
// assertions above. This is the only test anywhere that sees a revealed value,
// and it lives in internal/exec because no other package can reach the seam.
func TestSensitiveCommand_RevealReachesExecCmd(t *testing.T) {
	path := "/nonexistent/" + pathSentinel

	runner := &OSSensitiveRunner{env: []string{"PATH=/usr/bin", "CMUX_SOCKET_PATH=stale", "TMUX=/tmp/old,1,2"}}
	built := runner.buildCmd(SensitiveCommand{
		Kind:      KindTmuxCreate,
		Path:      Secret(path),
		Args:      []Arg{Opaque(arg0Sentinel), MustFixed("-t"), Opaque(arg1Sentinel)},
		Env:       []EnvMutation{ReplaceCmuxSocketPath(envSentinel), UnsetTmux()},
		StdoutCap: 1024,
		StderrCap: 1024,
	})

	if built.Path != path {
		t.Errorf("exec.Cmd.Path = %q, want %q", built.Path, path)
	}
	want := []string{path, arg0Sentinel, "-t", arg1Sentinel}
	if len(built.Args) != len(want) {
		t.Fatalf("exec.Cmd.Args = %q, want %q", built.Args, want)
	}
	for i := range want {
		if built.Args[i] != want[i] {
			t.Errorf("exec.Cmd.Args[%d] = %q, want %q", i, built.Args[i], want[i])
		}
	}

	env := strings.Join(built.Env, "\n")
	if !strings.Contains(env, "CMUX_SOCKET_PATH="+envSentinel) {
		t.Errorf("replacement env value did not reach exec.Cmd.Env: %q", built.Env)
	}
	if strings.Contains(env, "CMUX_SOCKET_PATH=stale") {
		t.Errorf("stale inherited entry survived the replacement: %q", built.Env)
	}
	if strings.Contains(env, "TMUX=") {
		t.Errorf("UnsetTmux did not remove the inherited TMUX: %q", built.Env)
	}
	if !strings.Contains(env, "PATH=/usr/bin") {
		t.Errorf("unrelated inherited entry was not preserved byte-exact: %q", built.Env)
	}
}

// TestOpaqueValues_RedactUnderEveryRenderingVerb pins each opaque type
// individually, so a regression in one type's method set is attributable
// rather than only visible through the aggregate leak test.
func TestOpaqueValues_RedactUnderEveryRenderingVerb(t *testing.T) {
	const secret = "SENTINEL-VALUE-z9y8x7"

	cases := map[string]any{
		"SecretArg":   Secret(secret),
		"Arg opaque":  Opaque(secret),
		"Arg fixed":   MustFixed("new-session"),
		"EnvMutation": ReplaceCmuxSocketPath(secret),
		"command": SensitiveCommand{
			Kind: KindCmuxCreate, Path: Secret(secret),
			Args: []Arg{Opaque(secret)}, Env: []EnvMutation{ReplaceCmuxSocketPath(secret)},
			StdoutCap: 1, StderrCap: 1,
		},
		"BoundedOutput": BoundedOutputForTest([]byte(secret), true),
		"result": SensitiveResult{
			Stdout: BoundedOutputForTest([]byte(secret), true),
			Stderr: BoundedOutputForTest([]byte(secret), false),
		},
		"error": newSensitiveError(KindCmuxCreate, OutcomeExit, SensitiveResult{}, "process reported a nonzero status"),
	}

	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"}
	for name, value := range cases {
		for _, verb := range verbs {
			if got := fmt.Sprintf(verb, value); strings.Contains(got, secret) {
				t.Errorf("%s under %s leaked: %s", name, verb, got)
			}
		}
		if got := mustMarshal(t, value); strings.Contains(got, secret) {
			t.Errorf("%s under json.Marshal leaked: %s", name, got)
		}
		if got := logLine(t, "v", value); strings.Contains(got, secret) {
			t.Errorf("%s under slog leaked: %s", name, got)
		}
	}
}

// TestFixedArg_RejectsUnsafeConstants proves the fixed-constant constructor is
// a validation boundary, not a label. Control characters are built from code
// points rather than written literally so the test file itself stays clean.
func TestFixedArg_RejectsUnsafeConstants(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"bell":          "new" + string(rune(0x07)) + "session",
		"escape":        string(rune(0x1B)) + "[31m",
		"newline":       "new\nsession",
		"delete":        "kill" + string(rune(0x7F)),
		"invalid utf-8": string([]byte{0xFF, 0xFE}),
		"oversize":      strings.Repeat("a", maxFixedArgBytes+1),
	}
	for name, value := range cases {
		if _, err := Fixed(value); err == nil {
			t.Errorf("Fixed(%s) was accepted; expected a refusal", name)
		} else if !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("Fixed(%s) returned %v, want ErrInvalidCommand", name, err)
		}
	}

	// The same values are legitimate as opaque dynamic values: a real path or
	// prompt may contain anything, which is why they are never rendered.
	for name, value := range cases {
		if a := Opaque(value); !a.set() || !a.Secret() {
			t.Errorf("Opaque(%s) should accept any payload", name)
		}
	}

	if _, err := Fixed("new-session"); err != nil {
		t.Errorf("Fixed rejected a legitimate constant: %v", err)
	}
}

// TestSensitiveCommand_ValidateRefusesBeforeStart covers every pre-start
// refusal. Each case must fail, and must fail as ErrInvalidCommand rather than
// by running something.
func TestSensitiveCommand_ValidateRefusesBeforeStart(t *testing.T) {
	base := func() SensitiveCommand {
		return SensitiveCommand{
			Kind:      KindTmuxProbe,
			Path:      Secret("/usr/bin/true"),
			Args:      []Arg{MustFixed("-V")},
			StdoutCap: 1024,
			StderrCap: 1024,
		}
	}

	cases := map[string]func(*SensitiveCommand){
		"zero kind":            func(c *SensitiveCommand) { c.Kind = KindUnspecified },
		"out-of-range kind":    func(c *SensitiveCommand) { c.Kind = CommandKind(200) },
		"unset path":           func(c *SensitiveCommand) { c.Path = SecretArg{} },
		"empty path":           func(c *SensitiveCommand) { c.Path = Secret("") },
		"relative path":        func(c *SensitiveCommand) { c.Path = Secret("true") },
		"dot-relative path":    func(c *SensitiveCommand) { c.Path = Secret("./true") },
		"unconstructed arg":    func(c *SensitiveCommand) { c.Args = []Arg{MustFixed("-V"), {}} },
		"dash-leading opaque":  func(c *SensitiveCommand) { c.Args = []Arg{Opaque("-rf")} },
		"zero env mutation":    func(c *SensitiveCommand) { c.Env = []EnvMutation{{}} },
		"empty replace value":  func(c *SensitiveCommand) { c.Env = []EnvMutation{ReplaceCmuxSocketPath("")} },
		"duplicate env key":    func(c *SensitiveCommand) { c.Env = []EnvMutation{SetCmuxQuiet(), SetCmuxQuiet()} },
		"zero stdout cap":      func(c *SensitiveCommand) { c.StdoutCap = 0 },
		"negative stdout cap":  func(c *SensitiveCommand) { c.StdoutCap = -1 },
		"stdout cap over ceil": func(c *SensitiveCommand) { c.StdoutCap = MaxOutputBytes + 1 },
		"zero stderr cap":      func(c *SensitiveCommand) { c.StderrCap = 0 },
		"stderr cap over ceil": func(c *SensitiveCommand) { c.StderrCap = MaxOutputBytes + 1 },
	}

	runner := NewOSSensitiveRunner()
	for name, mutate := range cases {
		cmd := base()
		mutate(&cmd)
		res, err := runner.RunSensitive(context.Background(), cmd)
		if err == nil {
			t.Errorf("%s was accepted; expected a pre-start refusal", name)
			continue
		}
		if !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s returned %v, want ErrInvalidCommand", name, err)
		}
		var se *SensitiveError
		if !errors.As(err, &se) || se.Outcome != OutcomeInvalid {
			t.Errorf("%s did not classify as OutcomeInvalid: %v", name, err)
		}
		if res.ExitCode != -1 {
			t.Errorf("%s reported ExitCode %d; a command that never ran has no exit status", name, res.ExitCode)
		}
		if res.Stdout.Len() != 0 || res.Stderr.Len() != 0 {
			t.Errorf("%s produced output despite refusing before start", name)
		}
	}

	// A dash-leading dynamic value is legitimate once an end-of-options
	// separator precedes it — the refusal above is a fail-closed default with
	// a returnable escape, not a wall.
	escaped := base()
	escaped.Args = []Arg{MustFixed("send-keys"), EndOfOptions(), Opaque("-rf")}
	if err := escaped.validate(); err != nil {
		t.Errorf("an end-of-options separator did not release the dash refusal: %v", err)
	}

	// A cap at the ceiling, and one narrower than it, are both legitimate.
	for _, limit := range []int64{1, MaxOutputBytes / 2, MaxOutputBytes} {
		cmd := base()
		cmd.StdoutCap, cmd.StderrCap = limit, limit
		if err := cmd.validate(); err != nil {
			t.Errorf("cap %d was refused: %v", limit, err)
		}
	}
}

// TestRunSensitive_RefusesAnAlreadyDoneContextWithoutForking pins that an
// expired deadline does not buy a fork/exec. The path is a real binary, so the
// only thing stopping it is the context check.
func TestRunSensitive_RefusesAnAlreadyDoneContextWithoutForking(t *testing.T) {
	runner, self := helperRunner(t, "ok:should-never-run", defaultRetireBound)

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	res, err := runner.RunSensitive(ctx, helperCommand(KindTmuxProbe, self, 1024))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res.Stdout.Len() != 0 {
		t.Errorf("the process ran: captured %d bytes", res.Stdout.Len())
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a command that never ran", res.ExitCode)
	}

	canceledCtx, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := runner.RunSensitive(canceledCtx, helperCommand(KindTmuxProbe, self, 1024)); !errors.Is(err, ErrCanceled) {
		t.Errorf("err = %v, want ErrCanceled", err)
	}
}

// TestOutcomes_RemainDistinguishable pins that each failure class unwraps to
// its own sentinel — a caller deciding whether to retry needs a timeout, an
// output-limit kill, and a cut-off stream to be different answers.
func TestOutcomes_RemainDistinguishable(t *testing.T) {
	pairs := []struct {
		outcome  Outcome
		sentinel error
	}{
		{OutcomeInvalid, ErrInvalidCommand},
		{OutcomeStartFailed, ErrStartFailed},
		{OutcomeExit, ErrNonzeroExit},
		{OutcomeTimeout, ErrTimeout},
		{OutcomeCanceled, ErrCanceled},
		{OutcomeOutputLimit, ErrOutputLimit},
		{OutcomeTruncated, ErrTruncated},
	}
	for _, p := range pairs {
		err := newSensitiveError(KindTmuxProbe, p.outcome, SensitiveResult{}, "")
		if !errors.Is(err, p.sentinel) {
			t.Errorf("%s did not match its own sentinel", p.outcome)
		}
		for _, other := range pairs {
			if other.outcome == p.outcome {
				continue
			}
			if errors.Is(err, other.sentinel) {
				t.Errorf("%s also matched %s; outcomes must stay distinguishable", p.outcome, other.outcome)
			}
		}
	}
}

// TestBoundedOutput_CopyIsFreshAndCarriesCompleteness proves a parser cannot
// mutate the runner's buffer and cannot receive partial bytes without also
// receiving the flag that says they are partial — for either cause.
func TestBoundedOutput_CopyIsFreshAndCarriesCompleteness(t *testing.T) {
	bo := BoundedOutputForTest([]byte("abc"), true)
	first, complete := bo.CopyBytesForParse()
	if !complete {
		t.Error("a complete output reported incomplete")
	}
	first[0] = 'z'
	second, _ := bo.CopyBytesForParse()
	if string(second) != "abc" {
		t.Errorf("mutating the copy changed the source: %q", second)
	}

	overflowed := BoundedOutputForTest([]byte("abc"), false)
	if _, complete := overflowed.CopyBytesForParse(); complete {
		t.Error("overflow output reported itself complete; it must never pass as a whole schema")
	}
	if overflowed.Complete() {
		t.Error("Complete() disagreed with CopyBytesForParse")
	}

	// A force-retired stream never hit its cap, so overflow is false — and it
	// must still report incomplete. Collapsing the two causes into one bool is
	// exactly how a cut-off prefix would reach a parser marked whole.
	cutOff := BoundedOutputForTest([]byte("abc"), true)
	cutOff.forced = true
	if cutOff.Complete() {
		t.Error("a force-retired stream reported itself complete")
	}
	if _, complete := cutOff.CopyBytesForParse(); complete {
		t.Error("force-retired bytes were offered to a parser as a complete schema")
	}

	var zero BoundedOutput
	if zero.Len() != 0 {
		t.Errorf("zero-value Len = %d, want 0", zero.Len())
	}
	if data, complete := zero.CopyBytesForParse(); len(data) != 0 || !complete {
		t.Errorf("zero-value CopyBytesForParse = %q, %v", data, complete)
	}
}

// TestFakeSensitiveRunner_ComparesOpaqueValues shows the intended adapter test
// shape: build the expected command with the same constructors and compare
// with Equal, with no path that turns a recorded argument back into a string.
func TestFakeSensitiveRunner_ComparesOpaqueValues(t *testing.T) {
	fake := &FakeSensitiveRunner{}
	args := []Arg{MustFixed("pane"), MustFixed("run"), Opaque("bootstrap-value")}
	sent := SensitiveCommand{
		Kind:      KindHerdrCreate,
		Path:      Secret("/opt/homebrew/bin/herdr"),
		Args:      args,
		Env:       []EnvMutation{ReplaceHerdrConfigPath("/tmp/herdr.toml")},
		StdoutCap: 4096,
		StderrCap: 4096,
	}
	if _, err := fake.RunSensitive(context.Background(), sent); err != nil {
		t.Fatalf("fake returned an error: %v", err)
	}

	// Reusing the backing array must not rewrite the recorded history.
	args[2] = Opaque("a-later-call-value")

	got, ok := fake.Last()
	if !ok {
		t.Fatal("fake recorded no call")
	}
	expected := SensitiveCommand{
		Kind:      KindHerdrCreate,
		Path:      Secret("/opt/homebrew/bin/herdr"),
		Args:      []Arg{MustFixed("pane"), MustFixed("run"), Opaque("bootstrap-value")},
		Env:       []EnvMutation{ReplaceHerdrConfigPath("/tmp/herdr.toml")},
		StdoutCap: 4096,
		StderrCap: 4096,
	}
	if !got.Equal(expected) {
		t.Error("the recorded call did not match the independently built expectation")
	}

	// A wrong value must actually fail the comparison, or the equality above
	// would be proving nothing.
	wrong := expected
	wrong.Args = []Arg{MustFixed("pane"), MustFixed("run"), Opaque("some-other-value")}
	if got.Equal(wrong) {
		t.Error("comparison treated different opaque values as equal")
	}
	// A fixed and an opaque argument with the same text are not the same
	// argument: the classification is part of the identity.
	if Opaque("run").Equal(MustFixed("run")) {
		t.Error("an opaque value compared equal to a fixed constant")
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func logLine(t *testing.T, key string, v any) string {
	t.Helper()
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	slog.New(dualHandler{a: slog.NewJSONHandler(&buf, opts), b: slog.NewTextHandler(&buf, opts)}).
		Info("probe", key, v)
	return buf.String()
}
