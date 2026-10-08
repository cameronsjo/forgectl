package herdr

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const testHerdrPath = "/opt/test/bin/herdr"

func notifyCmd(args ...exec.Arg) exec.SensitiveCommand {
	return exec.SensitiveCommand{
		Kind:      exec.KindHerdrNotify,
		Path:      exec.Secret(testHerdrPath),
		Args:      append([]exec.Arg{exec.MustFixed("notification"), exec.MustFixed("show")}, args...),
		StdoutCap: notifyStreamCap,
		StderrCap: notifyStreamCap,
	}
}

func TestNotificationShowBuildsTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    Notification
		want exec.SensitiveCommand
	}{
		{
			name: "title only",
			n:    Notification{Title: "desk: 17 merge done"},
			want: notifyCmd(exec.Opaque("desk: 17 merge done")),
		},
		{
			name: "body and sound",
			n:    Notification{Title: "Claude needs you", Body: "2 waiting", Sound: SoundRequest},
			want: notifyCmd(exec.MustFixed("--body"), exec.Opaque("2 waiting"),
				exec.MustFixed("--sound"), exec.MustFixed("request"), exec.Opaque("Claude needs you")),
		},
		{
			name: "dash-leading text cannot read as a flag",
			n:    Notification{Title: "--help", Body: "-rf", Sound: SoundDone},
			want: notifyCmd(exec.MustFixed("--body"), exec.Opaque(" -rf"),
				exec.MustFixed("--sound"), exec.MustFixed("done"), exec.Opaque(" --help")),
		},
		{
			name: "control characters are escaped, not passed",
			n:    Notification{Title: "a\x1b]0;pwned\x07b\nc", Sound: SoundNone},
			want: notifyCmd(exec.MustFixed("--sound"), exec.MustFixed("none"), exec.Opaque(`a\x1b]0;pwned\ab\nc`)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &exec.FakeSensitiveRunner{}
			if err := NotificationShow(t.Context(), run, testHerdrPath, tc.n); err != nil {
				t.Fatalf("NotificationShow: %v", err)
			}
			got, ok := run.Last()
			if !ok {
				t.Fatal("no command ran")
			}
			if !got.Equal(tc.want) {
				t.Errorf("command = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNotificationShowRefusesBeforeRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		n    Notification
	}{
		{"relative herdr path", "herdr", Notification{Title: "x"}},
		{"empty title", testHerdrPath, Notification{Title: "  "}},
		{"unknown sound", testHerdrPath, Notification{Title: "x", Sound: Sound(99)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &exec.FakeSensitiveRunner{}
			if err := NotificationShow(t.Context(), run, tc.path, tc.n); err == nil {
				t.Fatal("want an error")
			}
			if len(run.Calls()) != 0 {
				t.Error("a refused notification still ran")
			}
		})
	}
}

func TestNotificationShowCapsLongText(t *testing.T) {
	long := make([]byte, 5*NotificationMaxRunes)
	for i := range long {
		long[i] = 'x'
	}
	if got := notificationText(string(long)); len([]rune(got)) > NotificationMaxRunes+len([]rune(termsafe.TruncatedMarker)) {
		t.Errorf("text is %d runes, want at most %d plus the marker", len([]rune(got)), NotificationMaxRunes)
	}
}

func TestNotificationShowReadsHerdrRefusal(t *testing.T) {
	envelope := []byte(`{"error":{"code":"server_not_running","message":"no server"}}`)
	for _, tc := range []struct {
		name string
		res  exec.SensitiveResult
		err  error
	}{
		{
			name: "exit 1 with the envelope on stderr",
			res:  exec.SensitiveResult{ExitCode: exitFailure, Stderr: exec.BoundedOutputForTest(envelope, exec.OutputComplete)},
			err:  exec.SensitiveErrorForTest(exec.KindHerdrNotify, exec.OutcomeExit),
		},
		{
			name: "exit 0 with the envelope on stdout",
			res:  exec.SensitiveResult{Stdout: exec.BoundedOutputForTest(envelope, exec.OutputComplete)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
				return tc.res, tc.err
			}}
			err := NotificationShow(context.Background(), run, testHerdrPath, Notification{Title: "x"})
			var he *Error
			if !errors.As(err, &he) || he.Code != "server_not_running" {
				t.Fatalf("err = %v, want herdr's server_not_running refusal", err)
			}
		})
	}
}

func TestNotificationShowKeepsATruncatedStreamUnparsed(t *testing.T) {
	envelope := []byte(`{"error":{"code":"server_not_running","message":"no server"}}`)
	run := &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{ExitCode: exitFailure, Stderr: exec.BoundedOutputForTest(envelope, exec.OutputOverflowed)},
			exec.SensitiveErrorForTest(exec.KindHerdrNotify, exec.OutcomeExit)
	}}
	err := NotificationShow(t.Context(), run, testHerdrPath, Notification{Title: "x"})
	var he *Error
	if errors.As(err, &he) {
		t.Fatalf("a truncated stream was parsed as a refusal: %v", err)
	}
	if !errors.Is(err, exec.ErrNonzeroExit) {
		t.Errorf("err = %v, want the seam's nonzero-exit error", err)
	}
}
