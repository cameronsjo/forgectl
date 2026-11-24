package notify

// Test plan for notify.go
//
// Client.Notify (Classification: ops layer, subprocess sink)
//   [x] osascript (no terminal-notifier): one call with the constant script
//       and title and body as separate argv elements after "--"
//   [x] An AppleScript-injection body lands as one literal argv element
//   [x] Control characters, ESC sequences and bidi overrides are escaped, and
//       an overlong body is capped with termsafe's truncation marker
//   [x] Off darwin, Notify spawns nothing and returns nil
//   [x] A runner error is wrapped and rendered terminal-safe
//   [x] The runner sees a context with a deadline
//   [x] terminal-notifier: title, body, group, -execute and -activate argv
//   [x] A shell-injection body or title never reaches the -execute string
//   [x] A hostile terminal id drops -execute and keeps -activate
//   [x] -execute survives /bin/sh with a path holding a space and a quote
//   [x] No herdr-app: neither -execute nor -activate
//   [x] terminal-notifier failing falls back to osascript, reporting why
//   [x] Both senders failing returns both errors
//   [x] A terminal-notifier that hangs to its deadline leaves osascript time
//       inside the caller's deadline
//   [x] A value starting with a plist delimiter is escaped
// Client.Remove
//   [x] Runs -remove with the group; nothing without terminal-notifier or
//       for an invalid group
// GroupID
//   [x] Maps foreign runes to '-', caps at the id limit, and always validates

import (
	"context"
	"errors"
	osexec "os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	tnPath    = "/opt/homebrew/bin/terminal-notifier"
	appPath   = "/Users/x/.local/bin/herdr-app"
	selfPath  = "/opt/homebrew/bin/forgectl"
	errNotFnd = "not found"
)

// look resolves only the binaries named in found.
func look(found map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", errors.New(errNotFnd)
	}
}

func osascriptOnly(fake exec.Runner, extra ...Option) *Client {
	opts := append([]Option{WithGOOS("darwin"), WithLookPath(look(nil))}, extra...)
	return New(fake, opts...)
}

func withTN(fake exec.Runner, found map[string]string) *Client {
	return New(fake, WithGOOS("darwin"), WithLookPath(look(found)),
		WithExecutable(func() (string, error) { return selfPath, nil }))
}

func both() map[string]string {
	return map[string]string{SenderTerminalNotifier: tnPath, herdrAppBinary: appPath}
}

func TestNotify_OsascriptPassesTitleAndBodyAsArgv(t *testing.T) {
	fake := &exec.FakeRunner{}
	sent, err := osascriptOnly(fake).Notify(context.Background(), Message{Title: "Review started", Body: "o/r#1"})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if sent.Sender != SenderOsascript || sent.Fallback != nil {
		t.Errorf("sent = %+v, want osascript with no fallback", sent)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Name != "osascript" {
		t.Fatalf("calls = %+v, want one osascript call", fake.Calls)
	}
	want := []string{"-e", notifyScript, "--", "Review started", "o/r#1"}
	if strings.Join(fake.Calls[0].Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %q, want %q", fake.Calls[0].Args, want)
	}
}

func TestNotify_ScriptSourceIsConstantRegardlessOfInput(t *testing.T) {
	fake := &exec.FakeRunner{}
	body := `x" & do shell script "touch /tmp/p" & "`
	if _, err := osascriptOnly(fake).Notify(context.Background(), Message{Title: "Review started", Body: body}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	args := fake.Last().Args
	if len(args) != 5 || args[1] != notifyScript || args[4] != body {
		t.Errorf("args = %q, want the constant script and the body as one literal element", args)
	}
}

func TestNotify_ControlCharsEscapedAndLengthCapped(t *testing.T) {
	fake := &exec.FakeRunner{}
	body := "a\nb\x1b[31mred\u202eevil" + strings.Repeat("x", 1000)
	if _, err := osascriptOnly(fake).Notify(context.Background(), Message{Title: "t", Body: body}); err != nil {
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
		t.Errorf("body arg = %q, want the truncation marker", got)
	}
	if n := len([]rune(strings.TrimSuffix(got, termsafe.TruncatedMarker))); n > maxBodyRunes {
		t.Errorf("body arg is %d runes before the marker, want at most %d", n, maxBodyRunes)
	}
}

func TestNotify_NonDarwinIsNoOpNoSpawn(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("linux"), WithLookPath(look(both())))
	sent, err := c.Notify(context.Background(), Message{Title: "t", Body: "b"})
	if err != nil || sent != (Sent{}) {
		t.Fatalf("Notify = %+v, %v; want zero and nil", sent, err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %+v, want none off darwin", fake.Calls)
	}
	if c.Sender() != "" {
		t.Errorf("Sender = %q off darwin, want empty", c.Sender())
	}
}

func TestNotify_RunnerErrorIsWrappedAndSanitized(t *testing.T) {
	cause := errors.New("osascript failed: \x1b]0;pwned\x07 \x1b[2J")
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", cause }}
	_, err := osascriptOnly(fake).Notify(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("Notify: nil error, want the runner failure")
	}
	if strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("error text carries ESC: %q", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "osascript notification: ") || !errors.Is(err, cause) {
		t.Errorf("error = %q, want the osascript prefix unwrapping to the cause", err.Error())
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
	c := withTN(run, both())
	if _, err := c.Notify(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !run.hasDeadline {
		t.Error("the sender ran under a context with no deadline")
	}
	if c.timeout <= 0 || c.timeout > time.Minute {
		t.Errorf("timeout = %s, want a short positive bound", c.timeout)
	}
}

// flagValue is the element after flag in args, and whether flag is there.
func flagValue(args []string, flag string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func TestNotify_TerminalNotifierArgv(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := withTN(fake, both())
	sent, err := c.Notify(context.Background(), Message{Title: "forgectl desk: 1 waiting", Body: "item", Group: "forgectl-desk-item", Terminal: "t146"})
	if err != nil || sent.Sender != SenderTerminalNotifier {
		t.Fatalf("Notify = %+v, %v; want terminal-notifier", sent, err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Name != tnPath {
		t.Fatalf("calls = %+v, want one terminal-notifier call", fake.Calls)
	}
	args := fake.Calls[0].Args
	want := []string{
		"-title", `\forgectl desk: 1 waiting`, "-message", `\item`, "-group", "forgectl-desk-item",
		"-execute", `\'` + selfPath + "' 'herdr' '_focus-terminal' '--terminal' 't146' '--herdr-app' '" + appPath + "'",
		"-activate", herdrAppBundle,
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args =\n %q\nwant\n %q", args, want)
	}
}

func TestNotify_ExecuteNeverCarriesTitleOrBody(t *testing.T) {
	fake := &exec.FakeRunner{}
	hostile := `'; touch /tmp/pwned; echo '$(id)`
	m := Message{Title: hostile, Body: hostile, Group: "g", Terminal: "t1"}
	if _, err := withTN(fake, both()).Notify(context.Background(), m); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	args := fake.Last().Args
	exe, ok := flagValue(args, "-execute")
	if !ok {
		t.Fatalf("args = %q, want -execute", args)
	}
	if strings.Contains(exe, "pwned") || strings.Contains(exe, "$(id)") {
		t.Errorf("-execute = %q carries caller text", exe)
	}
	if msg, _ := flagValue(args, "-message"); msg != `\`+hostile {
		t.Errorf("-message = %q, want the body as one literal element", msg)
	}
}

func TestNotify_HostileTerminalDropsExecuteKeepsActivate(t *testing.T) {
	for _, term := range []string{"t1'; rm -rf ~; '", "t1 t2", "", strings.Repeat("a", 65), "t1\n"} {
		fake := &exec.FakeRunner{}
		if _, err := withTN(fake, both()).Notify(context.Background(), Message{Title: "t", Body: "b", Group: "bad group!", Terminal: term}); err != nil {
			t.Fatalf("Notify(%q): %v", term, err)
		}
		args := fake.Last().Args
		if _, ok := flagValue(args, "-execute"); ok {
			t.Errorf("terminal %q: args = %q, want no -execute", term, args)
		}
		if _, ok := flagValue(args, "-group"); ok {
			t.Errorf("terminal %q: args = %q, want no -group for an invalid group", term, args)
		}
		if v, _ := flagValue(args, "-activate"); v != herdrAppBundle {
			t.Errorf("terminal %q: args = %q, want -activate %s", term, args, herdrAppBundle)
		}
	}
}

func TestNotify_ExecuteSurvivesShellWithAwkwardPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	self := "/tmp/a dir/it's here/forgectl"
	app := "/tmp/o'brien/herdr-app"
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("darwin"),
		WithLookPath(look(map[string]string{SenderTerminalNotifier: tnPath, herdrAppBinary: app})),
		WithExecutable(func() (string, error) { return self, nil }))
	if _, err := c.Notify(context.Background(), Message{Title: "t", Body: "b", Terminal: "t146"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	exe, _ := flagValue(fake.Last().Args, "-execute")
	// terminal-notifier strips the leading backslash tnValue adds.
	exe = strings.TrimPrefix(exe, `\`)
	// The shell splits the string back into argv; printf shows each element.
	// The string under test is the input: running it through sh is the check.
	out, err := osexec.CommandContext(t.Context(), "/bin/sh", "-c", "set -- "+exe+"; printf '%s\\n' \"$@\"").Output() //nolint:gosec // G204: the test runs the shell string it is proving inert
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	want := strings.Join([]string{self, "herdr", "_focus-terminal", "--terminal", "t146", "--herdr-app", app}, "\n") + "\n"
	if string(out) != want {
		t.Errorf("sh split %q into\n%q\nwant\n%q", exe, out, want)
	}
}

func TestNotify_NoHerdrAppMeansNoClickTarget(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := withTN(fake, map[string]string{SenderTerminalNotifier: tnPath})
	if _, err := c.Notify(context.Background(), Message{Title: "t", Body: "b", Terminal: "t1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	args := fake.Last().Args
	for _, f := range []string{"-execute", "-activate"} {
		if _, ok := flagValue(args, f); ok {
			t.Errorf("args = %q, want no %s without herdr-app", args, f)
		}
	}
}

func TestNotify_TerminalNotifierFailureFallsBackToOsascript(t *testing.T) {
	refused := errors.New("exit status 3")
	fake := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == tnPath {
			return "", refused
		}
		return "", nil
	}}
	sent, err := withTN(fake, both()).Notify(context.Background(), Message{Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("Notify: %v, want nil after osascript posted", err)
	}
	if sent.Sender != SenderOsascript || !errors.Is(sent.Fallback, refused) {
		t.Errorf("sent = %+v, want osascript with terminal-notifier's failure as Fallback", sent)
	}
	if len(fake.Calls) != 2 || fake.Calls[1].Name != "osascript" {
		t.Errorf("calls = %+v, want terminal-notifier then osascript", fake.Calls)
	}
}

// hangFirstRunner blocks terminal-notifier until its context ends, and
// records whether osascript started with time left on its own context.
type hangFirstRunner struct {
	*exec.FakeRunner
	osaRan, osaLive bool
}

func (h *hangFirstRunner) Run(ctx context.Context, name string, _ ...string) (string, error) {
	if name == tnPath {
		<-ctx.Done()
		return "", ctx.Err()
	}
	h.osaRan, h.osaLive = true, ctx.Err() == nil
	return "", nil
}

func TestNotify_HungTerminalNotifierLeavesOsascriptTime(t *testing.T) {
	run := &hangFirstRunner{FakeRunner: &exec.FakeRunner{}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	sent, err := withTN(run, both()).Notify(ctx, Message{Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("Notify: %v, want osascript to post after terminal-notifier hung", err)
	}
	if !run.osaRan || !run.osaLive {
		t.Errorf("osascript ran=%v live=%v, want it run with time left", run.osaRan, run.osaLive)
	}
	if sent.Sender != SenderOsascript || !errors.Is(sent.Fallback, context.DeadlineExceeded) {
		t.Errorf("sent = %+v, want osascript with terminal-notifier's deadline as Fallback", sent)
	}
}

func TestNotify_BothSendersFailing(t *testing.T) {
	tnErr, osaErr := errors.New("tn down"), errors.New("osa down")
	fake := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == tnPath {
			return "", tnErr
		}
		return "", osaErr
	}}
	sent, err := withTN(fake, both()).Notify(context.Background(), Message{Title: "t", Body: "b"})
	if !errors.Is(err, tnErr) || !errors.Is(err, osaErr) {
		t.Errorf("err = %v, want both senders' failures", err)
	}
	if sent.Sender != "" {
		t.Errorf("sent = %+v, want no sender", sent)
	}
}

func TestRemove(t *testing.T) {
	fake := &exec.FakeRunner{}
	if err := withTN(fake, both()).Remove(context.Background(), "forgectl-desk-x"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Name != tnPath || strings.Join(fake.Calls[0].Args, " ") != "-remove forgectl-desk-x" {
		t.Errorf("calls = %+v, want terminal-notifier -remove forgectl-desk-x", fake.Calls)
	}

	none := &exec.FakeRunner{}
	if err := osascriptOnly(none).Remove(context.Background(), "forgectl-desk-x"); err != nil || len(none.Calls) != 0 {
		t.Errorf("Remove without terminal-notifier = %v, calls %+v; want nil and none", err, none.Calls)
	}
	bad := &exec.FakeRunner{}
	if err := withTN(bad, both()).Remove(context.Background(), "x; rm"); err != nil || len(bad.Calls) != 0 {
		t.Errorf("Remove of an invalid group = %v, calls %+v; want nil and none", err, bad.Calls)
	}
}

func TestGroupID(t *testing.T) {
	cases := map[string]string{
		"plain":    "forgectl-desk-plain",
		"a b/c.sh": "forgectl-desk-a-b-c-sh",
		"é'$(x)":   "forgectl-desk-----x-",
	}
	for key, want := range cases {
		if got := GroupID("desk", key); got != want {
			t.Errorf("GroupID(desk, %q) = %q, want %q", key, got, want)
		}
	}
	long := GroupID("desk", strings.Repeat("x", 500))
	if len(long) != maxIDLen || !ValidID(long) {
		t.Errorf("GroupID of a long key = %q (%d), want %d valid bytes", long, len(long), maxIDLen)
	}
}

func TestSender(t *testing.T) {
	if got := withTN(&exec.FakeRunner{}, both()).Sender(); got != SenderTerminalNotifier {
		t.Errorf("Sender with terminal-notifier = %q", got)
	}
	if got := osascriptOnly(&exec.FakeRunner{}).Sender(); got != SenderOsascript {
		t.Errorf("Sender without terminal-notifier = %q", got)
	}
}

func TestNotify_ValuesEscapedForPropertyListParsing(t *testing.T) {
	fake := &exec.FakeRunner{}
	if _, err := withTN(fake, both()).Notify(context.Background(), Message{Title: "(a, b)", Body: "[ZOMG] fire"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	args := fake.Last().Args
	if v, _ := flagValue(args, "-title"); v != `\(a, b)` {
		t.Errorf("-title = %q, want a leading backslash", v)
	}
	if v, _ := flagValue(args, "-message"); v != `\[ZOMG] fire` {
		t.Errorf("-message = %q, want a leading backslash", v)
	}
}
