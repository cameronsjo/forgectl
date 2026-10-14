package herdr

import (
	"context"
	"errors"
	"strings"
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
			want: notifyCmd(exec.Opaque("Claude needs you"), exec.MustFixed("--body"), exec.Opaque("2 waiting"),
				exec.MustFixed("--sound"), exec.MustFixed("request")),
		},
		{
			name: "dash-leading text cannot read as a flag",
			n:    Notification{Title: "--help", Body: "-rf", Sound: SoundDone},
			want: notifyCmd(exec.Opaque(" --help"), exec.MustFixed("--body"), exec.Opaque(" -rf"),
				exec.MustFixed("--sound"), exec.MustFixed("done")),
		},
		{
			name: "herdr's reserved -h title is not read as its help",
			n:    Notification{Title: "-h"},
			want: notifyCmd(exec.Opaque(" -h")),
		},
		{
			name: "a bare help title is not read as herdr's help",
			n:    Notification{Title: "help"},
			want: notifyCmd(exec.Opaque(" help")),
		},
		{
			name: "control characters are escaped, not passed",
			n:    Notification{Title: "a\x1b]0;pwned\x07b\nc", Sound: SoundNone},
			want: notifyCmd(exec.Opaque(`a\x1b]0;pwned\ab\nc`), exec.MustFixed("--sound"), exec.MustFixed("none")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &exec.FakeSensitiveRunner{}
			if _, err := NotificationShow(t.Context(), run, testHerdrPath, tc.n); err != nil {
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
			if _, err := NotificationShow(t.Context(), run, tc.path, tc.n); err == nil {
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
			_, err := NotificationShow(context.Background(), run, testHerdrPath, Notification{Title: "x"})
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
	_, err := NotificationShow(t.Context(), run, testHerdrPath, Notification{Title: "x"})
	var he *Error
	if errors.As(err, &he) {
		t.Fatalf("a truncated stream was parsed as a refusal: %v", err)
	}
	if !errors.Is(err, exec.ErrNonzeroExit) {
		t.Errorf("err = %v, want the seam's nonzero-exit error", err)
	}
}

// TestNotificationShowReadsShownAndReason pins herdr's answer: shown, a
// reason when it declined, and an exit-0 reply with no result read as shown.
func TestNotificationShowReadsShownAndReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		want  NotificationResult
	}{
		{"shown", `{"id":"cli:notification:show","result":{"type":"notification_show","shown":true,"reason":"shown"}}`, NotificationResult{Shown: true}},
		{"declined", `{"id":"x","result":{"type":"notification_show","shown":false,"reason":"no_foreground_client"}}`, NotificationResult{Reason: "no_foreground_client"}},
		{"declined without a reason", `{"id":"x","result":{"shown":false}}`, NotificationResult{Reason: "no reason given"}},
		{"declined with a hostile reason", `{"id":"x","result":{"shown":false,"reason":"busy\u001b[2J"}}`, NotificationResult{Reason: `busy\x1b[2J`}},
		{"no result", ``, NotificationResult{Shown: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
				return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest([]byte(tc.reply), exec.OutputComplete)}, nil
			}}
			got, err := NotificationShow(t.Context(), run, testHerdrPath, Notification{Title: "x"})
			if err != nil {
				t.Fatalf("NotificationShow: %v", err)
			}
			if got != tc.want {
				t.Errorf("result = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestNotificationShowCarriesHerdrsStderr: a failure that is not herdr's
// JSON refusal (a usage error exits 2 with plain text) carries herdr's own
// words, inert and redacted, so the operator need not run herdr by hand
// (forgectl#1181).
func TestNotificationShowCarriesHerdrsStderr(t *testing.T) {
	stderr := "unknown option: queued item\x1b]0;x\x07\nAWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n" //nolint:gosec // G101: a fake test credential shape
	run := &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{ExitCode: 2, Stderr: exec.BoundedOutputForTest([]byte(stderr), exec.OutputComplete)},
			exec.SensitiveErrorForTest(exec.KindHerdrNotify, exec.OutcomeExit)
	}}
	_, err := NotificationShow(t.Context(), run, testHerdrPath, Notification{Title: "x"})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "herdr said: unknown option: queued item") {
		t.Errorf("err = %q, want herdr's own stderr", msg)
	}
	if strings.ContainsAny(msg, "\x1b\x07\n") || strings.Contains(msg, "wJalrXUtnFEMI") {
		t.Errorf("err = %q, want it inert and redacted", msg)
	}
	if !errors.Is(err, exec.ErrNonzeroExit) {
		t.Errorf("err = %v, want the seam's nonzero-exit error kept", err)
	}
}

// TestNotificationShowScrubsItsOwnText: herdr echoes an operand it cannot
// place, so the title and body never reach an error through its stderr.
func TestNotificationShowScrubsItsOwnText(t *testing.T) {
	n := Notification{Title: "deploy prod-db", Body: "rotate secret-ABC then restart"}
	stderr := "unknown option: rotate secret-ABC then restart\nwhile showing deploy prod-db\n"
	run := &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{ExitCode: 2, Stderr: exec.BoundedOutputForTest([]byte(stderr), exec.OutputComplete)},
			exec.SensitiveErrorForTest(exec.KindHerdrNotify, exec.OutcomeExit)
	}}
	_, err := NotificationShow(t.Context(), run, testHerdrPath, n)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "secret-ABC") || strings.Contains(msg, "prod-db") {
		t.Fatalf("err = %q, carries the notification's own text", msg)
	}
	if !strings.Contains(msg, "unknown option: [redacted]") {
		t.Fatalf("err = %q, want herdr's words with the value scrubbed", msg)
	}
}

func TestScrubValuesOverlapsAndShortValues(t *testing.T) {
	for _, tc := range []struct {
		s    string
		hide []string
		want string
	}{
		{"x abcdef y", []string{"abcd", "cdef"}, "x [redacted] y"},
		{"a ab abc", []string{"a", "ab"}, "a ab abc"},
		{"no match", []string{"zzz"}, "no match"},
		{"aaaa", []string{"aaa"}, "[redacted]"},
	} {
		if got := scrubValues(tc.s, tc.hide); got != tc.want {
			t.Errorf("scrubValues(%q, %q) = %q, want %q", tc.s, tc.hide, got, tc.want)
		}
	}
}

// TestNotificationResultErr: every caller reads herdr's answer alike, and
// rate_limited is a note, not a failure.
func TestNotificationResultErr(t *testing.T) {
	if (NotificationResult{Shown: true}).Err() != nil || (NotificationResult{Shown: true}).Note() != "" {
		t.Error("a shown notification failed or carries a note")
	}
	rl := NotificationResult{Reason: ReasonRateLimited}
	if rl.Err() != nil || !strings.Contains(rl.Note(), "rate_limited") {
		t.Errorf("rate_limited: err %v, note %q; want no error and a note", rl.Err(), rl.Note())
	}
	if err := (NotificationResult{Reason: "busy"}).Err(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("busy: err %v, want a failure naming it", err)
	}
}

// TestNotificationShowScrubsItsTextFromARefusal: herdr's structured refusal
// message is scrubbed of the call's own text too.
func TestNotificationShowScrubsItsTextFromARefusal(t *testing.T) {
	n := Notification{Title: "deploy prod-db", Body: "rotate secret-ABC"}
	envelope := []byte(`{"error":{"code":"invalid_request","message":"cannot show rotate secret-ABC"}}`)
	for name, res := range map[string]exec.SensitiveResult{
		"exit 1": {ExitCode: exitFailure, Stderr: exec.BoundedOutputForTest(envelope, exec.OutputComplete)},
		"exit 0": {Stdout: exec.BoundedOutputForTest(envelope, exec.OutputComplete)},
	} {
		run := &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
			if res.ExitCode != 0 {
				return res, exec.SensitiveErrorForTest(exec.KindHerdrNotify, exec.OutcomeExit)
			}
			return res, nil
		}}
		_, err := NotificationShow(t.Context(), run, testHerdrPath, n)
		var he *Error
		if !errors.As(err, &he) || he.Code != "invalid_request" || strings.Contains(err.Error(), "secret-ABC") || !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("%s: err = %v, want the refusal with the body scrubbed", name, err)
		}
	}
}
