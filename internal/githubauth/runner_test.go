package githubauth

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// helperProcessEnv gates the re-exec helper below: the test binary re-runs
// itself with this set, and the helper test body exits nonzero only then.
const helperProcessEnv = "FORGECTL_GITHUBAUTH_HELPER_PROCESS"

// TestHelperProcessExitsNonzero is not a test. It is the child half of
// realExitError: re-executed with helperProcessEnv set, it exits 3 so the
// parent gets a genuine *os/exec.ExitError to feed through the wrapper.
func TestHelperProcessExitsNonzero(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		t.Skip("helper process; runs only under realExitError")
	}
	os.Exit(3)
}

// realExitError produces a genuine *os/exec.ExitError deterministically, by
// running the test binary in helper-process mode. Its point is what it does
// NOT carry: no context identity at all. Feeding it through the wrapper is
// what proves the ctx.Err()-first ordering, since errors.Is on this error can
// never match a context sentinel by accident.
func realExitError(t *testing.T) error {
	t.Helper()
	cmd := osexec.Command(os.Args[0], "-test.run=TestHelperProcessExitsNonzero")
	cmd.Env = append(os.Environ(), helperProcessEnv+"=1")
	err := cmd.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper process: want *exec.ExitError, got %T (%v)", err, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper exit error unexpectedly carries context identity: %v", err)
	}
	return err
}

// hookFake is a Runner double that runs a hook just before returning, so a
// test can cancel or expire the caller's context at the exact moment the
// subprocess "fails". exec.FakeRunner cannot do this: it ignores ctx entirely.
type hookFake struct {
	hook     func(context.Context)
	out      string
	err      error
	gotEnv   map[string]string
	gotUnset []string
	calls    int
}

func (f *hookFake) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	f.calls++
	if f.hook != nil {
		f.hook(ctx)
	}
	return f.out, f.err
}

func (f *hookFake) RunWithEnv(ctx context.Context, env map[string]string, _ string, _ ...string) (string, error) {
	f.calls++
	f.gotEnv = env
	if f.hook != nil {
		f.hook(ctx)
	}
	return f.out, f.err
}

func (f *hookFake) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, _ string, _ ...string) (string, error) {
	f.calls++
	f.gotEnv = env
	f.gotUnset = unset
	if f.hook != nil {
		f.hook(ctx)
	}
	return f.out, f.err
}

func (f *hookFake) RunWithInput(ctx context.Context, _ string, name string, args ...string) (string, error) {
	return f.Run(ctx, name, args...)
}

func (f *hookFake) RunInteractive(_ context.Context, _ string, _ ...string) error { return nil }

func TestRunner_PinsGhToGitHubComDespiteAmbientHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	fake := &exec.FakeRunner{}

	if _, err := Runner(fake, DefaultHost).Run(t.Context(), "gh", "api", "user"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	last := fake.Last()
	if last.Name != "gh" {
		t.Fatalf("command name = %q, want gh", last.Name)
	}
	if got := last.Env["GH_HOST"]; got != DefaultHost {
		t.Fatalf("GH_HOST = %q, want %q", got, DefaultHost)
	}
}

func TestRunner_DelegatesNonGhUntouched(t *testing.T) {
	fake := &exec.FakeRunner{}

	if _, err := Runner(fake, DefaultHost).Run(t.Context(), "git", "status"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	last := fake.Last()
	if last.Name != "git" {
		t.Fatalf("command name = %q, want git", last.Name)
	}
	// FakeRunner records Env only on RunWithEnv, so a nil Env proves the call
	// took the plain Run delegation path rather than the pinned one.
	if last.Env != nil {
		t.Fatalf("non-gh call carried env %v, want plain Run delegation", last.Env)
	}
}

// TestRunner_RunWithEnvCannotEscapeThePin covers the asymmetry a wrapper that
// overrode only Run would leave: RunWithEnv is the method for controlling a
// child's environment, so it is exactly where a caller could otherwise supply
// its own GH_HOST and route a gh call at an enterprise instance.
func TestRunner_RunWithEnvCannotEscapeThePin(t *testing.T) {
	fake := &exec.FakeRunner{}

	_, err := Runner(fake, DefaultHost).RunWithEnv(t.Context(),
		map[string]string{"GH_HOST": "ghe.example.test", "GH_TOKEN": "keep-me"},
		"gh", "api", "user")
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}

	last := fake.Last()
	if got := last.Env["GH_HOST"]; got != DefaultHost {
		t.Errorf("GH_HOST = %q, want %q — the caller's value must not win", got, DefaultHost)
	}
	if got := last.Env["GH_TOKEN"]; got != "keep-me" {
		t.Errorf("GH_TOKEN = %q, want the caller's other vars preserved", got)
	}
}

func TestRunner_RunWithEnvFilteredPreservesCallerRemovalAndCannotEscapePin(t *testing.T) {
	fake := &exec.FakeRunner{}
	callerUnset := []string{"CALLER_REMOVE", "GH_TOKEN"}

	_, err := Runner(fake, "github.example.com").RunWithEnvFiltered(t.Context(),
		map[string]string{"GH_HOST": "attacker.example", "GH_TOKEN": "leak-me", "KEEP": "value"},
		callerUnset, "gh", "api", "user")
	if err != nil {
		t.Fatalf("RunWithEnvFiltered: %v", err)
	}

	call := fake.Last()
	if got := call.Env["GH_HOST"]; got != "github.example.com" {
		t.Errorf("GH_HOST = %q, want configured host", got)
	}
	if got := call.Env["KEEP"]; got != "value" {
		t.Errorf("KEEP = %q, want caller override preserved", got)
	}
	assertTokenRemovals(t, call)
	if len(call.UnsetEnv) == 0 || call.UnsetEnv[0] != "CALLER_REMOVE" {
		t.Errorf("removals = %v, want caller removal preserved first", call.UnsetEnv)
	}
	if len(callerUnset) != 2 || callerUnset[0] != "CALLER_REMOVE" || callerUnset[1] != "GH_TOKEN" {
		t.Fatalf("caller-owned removal slice mutated: %v", callerUnset)
	}
}

func TestRunner_RunWithEnvFilteredDelegatesNonGhUntouched(t *testing.T) {
	fake := &exec.FakeRunner{}

	if _, err := Runner(fake, "github.example.com").RunWithEnvFiltered(t.Context(),
		map[string]string{"KEEP": "value"}, []string{"REMOVE"}, "git", "status"); err != nil {
		t.Fatalf("RunWithEnvFiltered(git): %v", err)
	}

	call := fake.Last()
	if _, present := call.Env["GH_HOST"]; present {
		t.Errorf("non-gh call gained GH_HOST: %v", call.Env)
	}
	if len(call.UnsetEnv) != 1 || call.UnsetEnv[0] != "REMOVE" {
		t.Errorf("non-gh removals = %v, want [REMOVE]", call.UnsetEnv)
	}
}

// TestRunner_RunWithInputRefusesGh covers the hole an embedded exec.Runner left
// open: RunWithInput was inherited unpinned, so a stdin-fed gh leg (the shape
// real `gh api graphql --input -` pagination would take) would have reached the
// subprocess on whatever host an ambient GH_HOST named. It cannot be pinned —
// stdin mode carries no environment — so the wrapper must refuse it, and must
// refuse it BEFORE any process is spawned.
func TestRunner_RunWithInputRefusesGh(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	fake := &exec.FakeRunner{}

	_, err := Runner(fake, DefaultHost).RunWithInput(t.Context(), "query{}", "gh", "api", "graphql", "--input", "-")

	if !errors.Is(err, ErrUnpinnableGhPath) {
		t.Fatalf("err = %v, want ErrUnpinnableGhPath", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("underlying calls = %d (%+v), want 0 — the refusal must precede the spawn", len(fake.Calls), fake.Calls)
	}
}

// TestRunner_RunInteractiveRefusesGh is RunWithInput's twin: the interactive
// mode takes no environment either, so an interactive gh cannot carry the pin.
func TestRunner_RunInteractiveRefusesGh(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	fake := &exec.FakeRunner{}

	err := Runner(fake, DefaultHost).RunInteractive(t.Context(), "gh", "auth", "login")

	if !errors.Is(err, ErrUnpinnableGhPath) {
		t.Fatalf("err = %v, want ErrUnpinnableGhPath", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("underlying calls = %d (%+v), want 0 — the refusal must precede the spawn", len(fake.Calls), fake.Calls)
	}
}

// TestRunner_NonGhStdinAndInteractiveStillDelegate keeps the refusal narrow:
// the pin is a GitHub-identity control, so pbcopy and tmux must be untouched.
func TestRunner_NonGhStdinAndInteractiveStillDelegate(t *testing.T) {
	fake := &exec.FakeRunner{}
	wrapped := Runner(fake, DefaultHost)

	if _, err := wrapped.RunWithInput(t.Context(), "clip me", "pbcopy"); err != nil {
		t.Fatalf("RunWithInput(pbcopy): %v", err)
	}
	if got := fake.Last(); got.Name != "pbcopy" || got.Input != "clip me" {
		t.Fatalf("pbcopy call = %+v, want name pbcopy with stdin preserved", got)
	}

	if err := wrapped.RunInteractive(t.Context(), "tmux", "attach-session"); err != nil {
		t.Fatalf("RunInteractive(tmux): %v", err)
	}
	if got := fake.Last(); got.Name != "tmux" || !got.Interactive {
		t.Fatalf("tmux call = %+v, want an interactive tmux delegation", got)
	}
}

func TestRunner_DoubleWrapIsHarmless(t *testing.T) {
	fake := &exec.FakeRunner{}

	if _, err := Runner(Runner(fake, DefaultHost), DefaultHost).Run(t.Context(), "gh", "api", "user"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("underlying calls = %d, want 1", len(fake.Calls))
	}
	if got := fake.Last().Env["GH_HOST"]; got != DefaultHost {
		t.Fatalf("GH_HOST = %q, want %q", got, DefaultHost)
	}
}

func TestRunner_CancelledContextBeatsDisagreeingRawError(t *testing.T) {
	raw := realExitError(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &hookFake{err: raw, out: "partial", hook: func(context.Context) { cancel() }}

	out, err := Runner(fake, DefaultHost).Run(ctx, "gh", "api", "user")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("err %q leaked the raw exit text", err)
	}
	// The wrapper passes the underlying runner's stdout return value through
	// untouched — it rewrites only the error. That is NOT a claim that a failed
	// command's stdout survives: OSRunner returns "" alongside a
	// *exec.CommandError, and classifyContextFailure drops that error whole
	// (Stderr and Output included) when a sentinel wins. hookFake returns a
	// non-empty string here precisely because OSRunner never would, which is
	// what makes the passthrough observable at all.
	if out != "partial" {
		t.Fatalf("stdout = %q, want the underlying return value passed through unmodified", out)
	}
}

func TestRunner_ExpiredDeadlineBeatsDisagreeingRawError(t *testing.T) {
	raw := realExitError(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	fake := &hookFake{err: raw, hook: func(ctx context.Context) { <-ctx.Done() }}

	_, err := Runner(fake, DefaultHost).Run(ctx, "gh", "api", "user")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Fatalf("err %q leaked the raw exit text", err)
	}
}

func TestRunner_LiveContextNormalizesRawContextCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  error
		want error
	}{
		{"wrapped deadline", errors.Join(errors.New("gh failed"), context.DeadlineExceeded), context.DeadlineExceeded},
		{"wrapped canceled", errors.Join(errors.New("gh failed"), context.Canceled), context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &hookFake{err: tc.raw}

			_, err := Runner(fake, DefaultHost).Run(t.Context(), "gh", "api", "user")

			if err != tc.want {
				t.Fatalf("err = %v (%T), want the bare sentinel %v", err, err, tc.want)
			}
		})
	}
}

func TestRunner_OrdinaryFailurePassesThroughUnchanged(t *testing.T) {
	raw := errors.New("gh: HTTP 401")
	fake := &hookFake{err: raw}

	_, err := Runner(fake, DefaultHost).Run(t.Context(), "gh", "api", "user")

	if !errors.Is(err, raw) {
		t.Fatalf("err = %v, want the raw error preserved for categorical classification", err)
	}
}

func TestSafeContextSentinel(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"canceled", context.Canceled, true},
		{"deadline", context.DeadlineExceeded, true},
		{"ordinary", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeContextSentinel(tc.err); got != tc.want {
				t.Fatalf("SafeContextSentinel(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestResolveHost(t *testing.T) {
	oversize := strings.Repeat("a", MaxHostBytes+1)
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty means default", "", DefaultHost, false},
		{"whitespace only means default", "   \t", DefaultHost, false},
		{"github.com passes", "github.com", "github.com", false},
		{"ghe host passes", "github.example-corp.com", "github.example-corp.com", false},
		{"uppercase is lowercased", "GitHub.Example.COM", "github.example.com", false},
		{"surrounding whitespace trimmed", "  github.example.com  ", "github.example.com", false},
		{"exactly MaxHostBytes passes", strings.Repeat("a", MaxHostBytes), strings.Repeat("a", MaxHostBytes), false},
		{"port rejected", "github.example.com:8443", "", true},
		{"scheme rejected", "https://github.example.com", "", true},
		{"path rejected", "github.example.com/api", "", true},
		{"interior whitespace rejected", "github example.com", "", true},
		{"leading hyphen rejected", "-github.com", "", true},
		{"trailing dot rejected", "github.com.", "", true},
		{"oversize rejected", oversize, "", true},
		{"env-shaped value rejected", "github.com\nGH_TOKEN=x", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveHost(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveHost(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ResolveHost(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}

// TestResolveHost_ErrorNeverRendersValue is the leak test: a hostile config
// line must not reach a terminal via its own rejection message.
func TestResolveHost_ErrorNeverRendersValue(t *testing.T) {
	hostile := "EVIL-$(id)-github.com:443/pwn"
	_, err := ResolveHost(hostile)
	if err == nil {
		t.Fatal("want rejection")
	}
	if strings.Contains(err.Error(), "EVIL") || strings.Contains(err.Error(), "id)") {
		t.Fatalf("error %q renders the rejected value", err)
	}
}

// TestRunner_InvalidHostFailsClosedOnEveryGhPath: a Runner built over a host
// that fails validation must refuse gh everywhere, before any spawn, while
// non-gh commands still delegate.
func TestRunner_InvalidHostFailsClosedOnEveryGhPath(t *testing.T) {
	fake := &exec.FakeRunner{}
	wrapped := Runner(fake, "ghe.example.com:8443")

	if _, err := wrapped.Run(t.Context(), "gh", "api", "user"); !errors.Is(err, ErrUnpinnableHost) {
		t.Fatalf("Run err = %v, want ErrUnpinnableHost", err)
	}
	if _, err := wrapped.RunWithEnv(t.Context(), nil, "gh", "api", "user"); !errors.Is(err, ErrUnpinnableHost) {
		t.Fatalf("RunWithEnv err = %v, want ErrUnpinnableHost", err)
	}
	if _, err := wrapped.RunWithEnvFiltered(t.Context(), nil, nil, "gh", "api", "user"); !errors.Is(err, ErrUnpinnableHost) {
		t.Fatalf("RunWithEnvFiltered err = %v, want ErrUnpinnableHost", err)
	}
	if _, err := wrapped.RunWithInput(t.Context(), "q", "gh", "api", "graphql"); !errors.Is(err, ErrUnpinnableHost) {
		t.Fatalf("RunWithInput err = %v, want ErrUnpinnableHost", err)
	}
	if err := wrapped.RunInteractive(t.Context(), "gh", "auth", "status"); !errors.Is(err, ErrUnpinnableHost) {
		t.Fatalf("RunInteractive err = %v, want ErrUnpinnableHost", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("underlying gh calls = %d (%+v), want 0 — the refusal must precede the spawn", len(fake.Calls), fake.Calls)
	}
	if _, err := wrapped.Run(t.Context(), "git", "status"); err != nil {
		t.Fatalf("non-gh delegation: %v", err)
	}
	if got := fake.Last(); got.Name != "git" {
		t.Fatalf("non-gh call = %+v, want git delegation despite the invalid host", got)
	}
}

// TestRunner_ConfiguredHostPinBeatsAmbientAndCallerEnv: the pin stays total
// with a configured host — ambient GH_HOST and a caller-supplied GH_HOST both
// lose to the validated config value.
func TestRunner_ConfiguredHostPinBeatsAmbientAndCallerEnv(t *testing.T) {
	t.Setenv("GH_HOST", "ambient.example.test")
	fake := &exec.FakeRunner{}
	const ghe = "github.example.com"

	_, err := Runner(fake, ghe).RunWithEnv(t.Context(),
		map[string]string{"GH_HOST": "caller.example.test"}, "gh", "api", "user")
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}
	if got := fake.Last().Env["GH_HOST"]; got != ghe {
		t.Fatalf("GH_HOST = %q, want the configured %q", got, ghe)
	}
}

// TestRunner_NonDefaultHostRemovesTokens: gh sends GH_ENTERPRISE_TOKEN to
// whatever GH_HOST names and GH_TOKEN/GITHUB_TOKEN to github.com and
// *.ghe.com, so on any non-default host all four credential variables are
// absent — an ambient or caller-supplied token must never reach a gh
// subprocess pointed at a configured host.
func TestRunner_NonDefaultHostRemovesTokens(t *testing.T) {
	fake := &exec.FakeRunner{}

	_, err := Runner(fake, "github.example.com").RunWithEnv(t.Context(),
		map[string]string{"GH_TOKEN": "leak-me", "GH_ENTERPRISE_TOKEN": "leak-me-too"},
		"gh", "api", "user")
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}

	call := fake.Last()
	assertTokenRemovals(t, call)
	if got := call.Env["GH_HOST"]; got != "github.example.com" {
		t.Errorf("GH_HOST = %q, want github.example.com", got)
	}
}

// TestRunner_DefaultHostLeavesTokensUntouched: the scrub is bound to the
// non-default case — on github.com the operator's own tokens keep working.
func TestRunner_DefaultHostLeavesTokensUntouched(t *testing.T) {
	fake := &exec.FakeRunner{}

	_, err := Runner(fake, "").RunWithEnv(t.Context(),
		map[string]string{"GH_TOKEN": "keep-me"}, "gh", "api", "user")
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}

	env := fake.Last().Env
	if got := env["GH_TOKEN"]; got != "keep-me" {
		t.Errorf("GH_TOKEN = %q, want the caller's value preserved on the default host", got)
	}
	for _, k := range []string{"GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		if _, present := env[k]; present {
			t.Errorf("%s injected on the default host, want absent", k)
		}
	}
}

// TestRunner_ConfiguredHostRunRemovesTokensToo: the plain Run path (no caller
// env) gets the same removal as RunWithEnv on a non-default host.
func TestRunner_ConfiguredHostRunRemovesTokensToo(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-token")
	fake := &exec.FakeRunner{}

	if _, err := Runner(fake, "github.example.com").Run(t.Context(), "gh", "api", "user"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertTokenRemovals(t, fake.Last())
}

func assertTokenRemovals(t *testing.T, call exec.Call) {
	t.Helper()
	counts := make(map[string]int, len(call.UnsetEnv))
	for _, key := range call.UnsetEnv {
		counts[key]++
	}
	for _, key := range tokenEnvVars {
		if value, present := call.Env[key]; present {
			t.Errorf("%s remains in overrides with value %q, want absent", key, value)
		}
		if counts[key] != 1 {
			t.Errorf("%s removal count = %d in %v, want exactly 1", key, counts[key], call.UnsetEnv)
		}
	}
}

// TestRunner_DoubleWrapSameHostIsHarmless mirrors the default-host double-wrap
// test for a configured host — ResolveOwners wraps an already-wrapped runner
// on the review path, so idempotency is load-bearing.
func TestRunner_DoubleWrapSameHostIsHarmless(t *testing.T) {
	fake := &exec.FakeRunner{}
	const ghe = "github.example.com"

	if _, err := Runner(Runner(fake, ghe), ghe).Run(t.Context(), "gh", "api", "user"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("underlying calls = %d, want 1", len(fake.Calls))
	}
	if got := fake.Last().Env["GH_HOST"]; got != ghe {
		t.Fatalf("GH_HOST = %q, want %q", got, ghe)
	}
}

// TestRunner_DoubleWrapDifferentHostsFailsClosed: nesting two live pins with
// disagreeing hosts must not run at all — each layer sets GH_HOST last before
// delegating, so the innermost host would win while the outer layer's token
// scrub decision stood. Nobody chose one identity; refuse every gh path.
func TestRunner_DoubleWrapDifferentHostsFailsClosed(t *testing.T) {
	fake := &exec.FakeRunner{}
	wrapped := Runner(Runner(fake, "github.example.com"), "")

	if _, err := wrapped.Run(t.Context(), "gh", "api", "user"); !errors.Is(err, ErrUnpinnableHost) {
		t.Fatalf("err = %v, want ErrUnpinnableHost on host-conflicting double wrap", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("underlying calls = %d, want 0", len(fake.Calls))
	}
	if _, err := wrapped.Run(t.Context(), "git", "status"); err != nil {
		t.Fatalf("non-gh delegation: %v", err)
	}
}

// TestRunner_RunStreamingRefusesGh: exec.StreamingRunner is a separate
// interface, so a pinned runner must implement it and refuse gh — otherwise a
// future streaming gh leg would be written against the unwrapped runner,
// outside the pin and the scrub.
func TestRunner_RunStreamingRefusesGh(t *testing.T) {
	fake := &exec.FakeRunner{}
	sr, ok := Runner(fake, DefaultHost).(exec.StreamingRunner)
	if !ok {
		t.Fatal("pinned runner does not satisfy exec.StreamingRunner")
	}
	err := sr.RunStreaming(t.Context(), strings.NewReader(""), &strings.Builder{}, &strings.Builder{}, "gh", "api", "graphql")
	if !errors.Is(err, ErrUnpinnableGhPath) {
		t.Fatalf("err = %v, want ErrUnpinnableGhPath", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("underlying calls = %d, want 0", len(fake.Calls))
	}
}
