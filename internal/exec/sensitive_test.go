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
)

// captureSlog installs a debug-level JSON handler over a buffer as the default
// logger for the duration of the test and returns the buffer. Debug level
// matters: the leak this seam exists to close was a slog.Debug call, so a
// capture at Info would prove nothing.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestSensitiveCommand_SentinelNeverRendersThroughAnyPath is the load-bearing
// test. Every value that goes into the command carries a unique sentinel; the
// command is run against a binary that cannot start; and the resulting error,
// result, command, and captured log are then rendered through every path a
// caller or a logger could plausibly take. No sentinel may appear in any of
// them.
func TestSensitiveCommand_SentinelNeverRendersThroughAnyPath(t *testing.T) {
	logs := captureSlog(t)

	const (
		pathSentinel  = "SENTINEL-PATH-a1b2c3"
		arg0Sentinel  = "SENTINEL-ARG0-d4e5f6"
		arg1Sentinel  = "SENTINEL-ARG1-g7h8i9"
		arg2Sentinel  = "SENTINEL-ARG2-j1k2l3"
		envSentinel   = "SENTINEL-ENV-m4n5o6"
		herdrSentinel = "SENTINEL-HERDR-p7q8r9"
	)
	sentinels := []string{pathSentinel, arg0Sentinel, arg1Sentinel, arg2Sentinel, envSentinel, herdrSentinel}

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

	renderings := map[string]string{
		"err.Error()":             err.Error(),
		"%v of err":               fmt.Sprintf("%v", err),
		"%+v of err":              fmt.Sprintf("%+v", err),
		"%#v of err":              fmt.Sprintf("%#v", err),
		"%q of err":               fmt.Sprintf("%q", err),
		"%s of err":               fmt.Sprintf("%s", err),
		"errors.Unwrap(err)":      fmt.Sprintf("%v", errors.Unwrap(err)),
		"%v of result":            fmt.Sprintf("%v", res),
		"%+v of result":           fmt.Sprintf("%+v", res),
		"%#v of result":           fmt.Sprintf("%#v", res),
		"%v of command":           fmt.Sprintf("%v", cmd),
		"%+v of command":          fmt.Sprintf("%+v", cmd),
		"%#v of command":          fmt.Sprintf("%#v", cmd),
		"%q of command":           fmt.Sprintf("%q", cmd),
		"%v of command args":      fmt.Sprintf("%v", cmd.Args),
		"%+v of command env":      fmt.Sprintf("%+v", cmd.Env),
		"%#v of command path":     fmt.Sprintf("%#v", cmd.Path),
		"json.Marshal(err)":       mustMarshal(t, err),
		"json.Marshal(result)":    mustMarshal(t, res),
		"json.Marshal(command)":   mustMarshal(t, cmd),
		"json.Marshal(args)":      mustMarshal(t, cmd.Args),
		"json.Marshal(env)":       mustMarshal(t, cmd.Env),
		"slog of err":             logLine(t, "err", err),
		"slog of result":          logLine(t, "res", res),
		"slog of command":         logLine(t, "cmd", cmd),
		"slog of path":            logLine(t, "path", cmd.Path),
		"slog of args":            logLine(t, "args", cmd.Args),
		"slog of env":             logLine(t, "env", cmd.Env),
		"captured runner logging": logs.String(),
	}

	for name, rendered := range renderings {
		for _, s := range sentinels {
			if strings.Contains(rendered, s) {
				t.Errorf("%s leaked sentinel %s: %s", name, s, rendered)
			}
		}
	}

	// The captured logging must be non-empty, or "no sentinel appeared" would
	// be satisfied by the runner having logged nothing at all.
	if logs.Len() == 0 {
		t.Fatal("runner produced no log output; the leak assertion would be vacuous")
	}
	if !strings.Contains(logs.String(), "tmux.create") {
		t.Errorf("expected the closed command kind in the logs, got: %s", logs.String())
	}
}

// TestSensitiveCommand_RevealReachesExecCmd is the mirror of the leak test.
// Without it, a seam that dropped every value on the floor would pass the
// assertion above. This is the only test anywhere that sees a revealed value,
// and it lives in internal/exec because no other package can reach the seam.
func TestSensitiveCommand_RevealReachesExecCmd(t *testing.T) {
	const (
		pathSentinel = "SENTINEL-PATH-a1b2c3"
		arg0Sentinel = "SENTINEL-ARG0-d4e5f6"
		arg1Sentinel = "SENTINEL-ARG1-g7h8i9"
		envSentinel  = "SENTINEL-ENV-m4n5o6"
	)
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
		if a := Opaque(value); !a.set || !a.Secret() {
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
		"unconstructed arg":    func(c *SensitiveCommand) { c.Args = []Arg{MustFixed("-V"), {}} },
		"zero env mutation":    func(c *SensitiveCommand) { c.Env = []EnvMutation{{}} },
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
		if res.Stdout.Len() != 0 || res.Stderr.Len() != 0 {
			t.Errorf("%s produced output despite refusing before start", name)
		}
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

// TestOutcomes_RemainDistinguishable pins that each failure class unwraps to
// its own sentinel — a caller deciding whether to retry needs a timeout and an
// output-limit kill to be different answers.
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
// mutate the runner's buffer and cannot receive truncated bytes without also
// receiving the flag that says they are truncated.
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

	truncated := BoundedOutputForTest([]byte("abc"), false)
	if _, complete := truncated.CopyBytesForParse(); complete {
		t.Error("overflow output reported itself complete; it must never pass as a whole schema")
	}
	if truncated.Complete() {
		t.Error("Complete() disagreed with CopyBytesForParse")
	}
}

// TestFakeSensitiveRunner_ComparesOpaqueValues shows the intended adapter test
// shape: build the expected command with the same constructors and compare,
// with no path that turns a recorded argument back into a string.
func TestFakeSensitiveRunner_ComparesOpaqueValues(t *testing.T) {
	fake := &FakeSensitiveRunner{}
	want := SensitiveCommand{
		Kind:      KindHerdrCreate,
		Path:      Secret("/opt/homebrew/bin/herdr"),
		Args:      []Arg{MustFixed("pane"), MustFixed("run"), Opaque("bootstrap-value")},
		Env:       []EnvMutation{ReplaceHerdrConfigPath("/tmp/herdr.toml")},
		StdoutCap: 4096,
		StderrCap: 4096,
	}
	if _, err := fake.RunSensitive(context.Background(), want); err != nil {
		t.Fatalf("fake returned an error: %v", err)
	}

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
	if got.Kind != expected.Kind || got.Path != expected.Path {
		t.Errorf("recorded kind/path did not match the independently built expectation")
	}
	for i := range expected.Args {
		if got.Args[i] != expected.Args[i] {
			t.Errorf("recorded arg %d did not match the independently built expectation", i)
		}
	}
	for i := range expected.Env {
		if got.Env[i] != expected.Env[i] {
			t.Errorf("recorded env %d did not match the independently built expectation", i)
		}
	}

	// A wrong value must actually fail the comparison, or the equality above
	// would be proving nothing.
	if got.Args[2] == Opaque("some-other-value") {
		t.Error("comparison treated different opaque values as equal")
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
	slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})).
		Info("probe", key, v)
	var text bytes.Buffer
	slog.New(slog.NewTextHandler(&text, &slog.HandlerOptions{Level: slog.LevelDebug})).
		Info("probe", key, v)
	return buf.String() + text.String()
}
