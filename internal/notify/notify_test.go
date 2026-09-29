package notify

// Test plan for notify.go
//
// Client.Notify (Classification: ops layer, subprocess sink)
//   [x] On darwin, one osascript call carries the constant script and the
//       title and body as separate argv elements after "--"
//   [x] An AppleScript-injection body lands as one literal argv element and
//       the script element stays the constant
//   [x] Control characters, ESC sequences and bidi overrides are escaped, and
//       an overlong body is capped with termsafe's truncation marker
//   [x] Off darwin, Notify spawns nothing and returns nil
//   [x] A runner error is wrapped and rendered terminal-safe
//   [x] The runner sees a context with a deadline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func TestNotify_DarwinPassesTitleAndBodyAsArgv(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("darwin"))

	if err := c.Notify(context.Background(), "Review started", "o/r#1"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.Calls))
	}
	call := fake.Calls[0]
	if call.Name != "osascript" {
		t.Errorf("name = %q, want osascript", call.Name)
	}
	want := []string{"-e", notifyScript, "--", "Review started", "o/r#1"}
	if strings.Join(call.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %q, want %q", call.Args, want)
	}
}

func TestNotify_ScriptSourceIsConstantRegardlessOfInput(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("darwin"))
	body := `x" & do shell script "touch /tmp/p" & "`

	if err := c.Notify(context.Background(), "Review started", body); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	args := fake.Last().Args
	if len(args) != 5 {
		t.Fatalf("args = %q, want 5 elements", args)
	}
	if args[1] != notifyScript {
		t.Errorf("script = %q, want the constant notifyScript", args[1])
	}
	if args[4] != body {
		t.Errorf("body arg = %q, want the input as one literal element %q", args[4], body)
	}
}

func TestNotify_ControlCharsEscapedAndLengthCapped(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("darwin"))
	body := "a\nb\x1b[31mred\u202eevil" + strings.Repeat("x", 1000)

	if err := c.Notify(context.Background(), "Review started", body); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	got := fake.Last().Args[4]
	for i := 0; i < len(got); i++ {
		if got[i] < 0x20 {
			t.Fatalf("body arg carries control byte %#x at %d: %q", got[i], i, got)
		}
	}
	if strings.ContainsRune(got, '\u202e') {
		t.Errorf("body arg carries U+202E: %q", got)
	}
	if !strings.HasSuffix(got, termsafe.TruncatedMarker) {
		t.Errorf("body arg = %q, want it to end with the truncation marker", got)
	}
	if n := len([]rune(strings.TrimSuffix(got, termsafe.TruncatedMarker))); n > maxBodyRunes {
		t.Errorf("body arg is %d runes before the marker, want at most %d", n, maxBodyRunes)
	}
}

func TestNotify_NonDarwinIsNoOpNoSpawn(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("linux"))

	if err := c.Notify(context.Background(), "Review started", "o/r#1"); err != nil {
		t.Fatalf("Notify: %v, want nil", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %+v, want none off darwin", fake.Calls)
	}
}

func TestNotify_RunnerErrorIsWrappedAndSanitized(t *testing.T) {
	cause := errors.New("osascript failed: \x1b]0;pwned\x07 \x1b[2J")
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", cause }}
	c := New(fake, WithGOOS("darwin"))

	err := c.Notify(context.Background(), "Review started", "o/r#1")
	if err == nil {
		t.Fatal("Notify: nil error, want the runner failure")
	}
	if strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("error text carries ESC: %q", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "osascript notification: ") {
		t.Errorf("error = %q, want the osascript notification prefix", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Errorf("error does not unwrap to the runner cause")
	}
}

// deadlineRunner records whether the context Run received carried a deadline.
type deadlineRunner struct {
	*exec.FakeRunner
	hasDeadline bool
}

func (d *deadlineRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	_, d.hasDeadline = ctx.Deadline()
	return d.FakeRunner.Run(ctx, name, args...)
}

func TestNotify_TimeoutBoundsContext(t *testing.T) {
	run := &deadlineRunner{FakeRunner: &exec.FakeRunner{}}
	c := New(run, WithGOOS("darwin"))

	if err := c.Notify(context.Background(), "Review started", "o/r#1"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !run.hasDeadline {
		t.Error("osascript ran under a context with no deadline; a hung notification would block the caller")
	}
	if c.timeout <= 0 || c.timeout > time.Minute {
		t.Errorf("timeout = %s, want a short positive bound", c.timeout)
	}
}
