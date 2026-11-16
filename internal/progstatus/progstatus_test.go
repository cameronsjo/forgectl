package progstatus

import (
	"encoding/base64"
	"io"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/meta"
)

func forceTTY(t *testing.T, v bool) {
	t.Helper()
	old := isTerminal
	isTerminal = func(uintptr) bool { return v }
	t.Cleanup(func() { isTerminal = old })
}

// capture returns a Reporter over a pipe and a func that drains what it wrote.
func capture(t *testing.T, env map[string]string) (*Reporter, func() string) {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	r := New(wr, func(k string) string { return env[k] })
	return r, func() string {
		_ = wr.Close()
		b, err := io.ReadAll(rd)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestExactBytes(t *testing.T) {
	forceTTY(t, true)
	const pre = "\x1b]7501;"
	const idApp = ":id=forgectl:app=forgectl"
	cases := []struct {
		name string
		do   func(*Reporter)
		want string
	}{
		{"working with progress", func(r *Reporter) { r.Working("wf 1/3 run", 33) }, pre + "state=working" + idApp + ":progress=33:msg=" + b64("wf 1/3 run") + "\x1b\\"},
		{"working without progress", func(r *Reporter) { r.Working("x", -1) }, pre + "state=working" + idApp + ":msg=" + b64("x") + "\x1b\\"},
		{"done", func(r *Reporter) { r.Done("fin") }, pre + "state=done" + idApp + ":msg=" + b64("fin") + "\x1b\\"},
		{"error", func(r *Reporter) { r.Error("bad") }, pre + "state=error" + idApp + ":msg=" + b64("bad") + "\x1b\\"},
		{"blocked auth", func(r *Reporter) { r.Blocked(KindAuth, "Touch ID") }, pre + "state=blocked" + idApp + ":kind=auth:msg=" + b64("Touch ID") + "\x1b\\"},
		{"blocked permission", func(r *Reporter) { r.Blocked(KindPermission, "2 waiting") }, pre + "state=blocked" + idApp + ":kind=permission:msg=" + b64("2 waiting") + "\x1b\\"},
		{"blocked question", func(r *Reporter) { r.Blocked(KindQuestion, "name?") }, pre + "state=blocked" + idApp + ":kind=question:msg=" + b64("name?") + "\x1b\\"},
		{"blocked no kind", func(r *Reporter) { r.Blocked(KindNone, "ok?") }, pre + "state=blocked" + idApp + ":msg=" + b64("ok?") + "\x1b\\"},
		{"blocked unknown kind sends no kind key", func(r *Reporter) { r.Blocked(Kind("approval"), "ok?") }, pre + "state=blocked" + idApp + ":msg=" + b64("ok?") + "\x1b\\"},
		{"blocked kind cannot inject keys", func(r *Reporter) { r.Blocked(Kind("auth:id=x"), "ok?") }, pre + "state=blocked" + idApp + ":msg=" + b64("ok?") + "\x1b\\"},
		{"clear carries id", func(r *Reporter) { r.Clear() }, pre + "state=clear" + idApp + "\x1b\\"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, out := capture(t, nil)
			tc.do(r)
			if got := out(); got != tc.want {
				t.Errorf("bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisabledWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		tty  bool
		env  map[string]string
	}{
		{"no tty", false, nil},
		{"TERM=dumb", true, map[string]string{"TERM": "dumb"}},
		{"opt-out", true, map[string]string{EnvOptOut: "1"}},
		{"FORGECTL_WORKER", true, map[string]string{"FORGECTL_WORKER": "probe"}},
		{"FORGECTL_DRAIN_WORKER", true, map[string]string{"FORGECTL_DRAIN_WORKER": "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forceTTY(t, tc.tty)
			r, out := capture(t, tc.env)
			r.Working("x", 10)
			r.Done("x")
			r.Clear()
			if got := out(); got != "" {
				t.Errorf("wrote %q, want nothing", got)
			}
		})
	}
}

func TestNilAndZeroReporterAreNoOps(t *testing.T) {
	var r *Reporter
	r.Done("x")
	(&Reporter{}).Error("x")
}

func TestTmuxPassthrough(t *testing.T) {
	forceTTY(t, true)
	r, out := capture(t, map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"})
	r.Done("x")
	// Inside tmux's DCS passthrough every ESC is doubled and the whole thing ends in ST.
	inner := "\x1b\x1b]7501;state=done:id=forgectl:app=forgectl:msg=" + b64("x") + "\x1b\x1b\\"
	want := "\x1bPtmux;" + inner + "\x1b\\"
	if got := out(); got != want {
		t.Errorf("tmux output = %q, want %q", got, want)
	}
}

func TestHostileMsgHasNoControlRunes(t *testing.T) {
	forceTTY(t, true)
	r, out := capture(t, nil)
	r.Error("a\x1b]0;pwn\x07b\nc\u202ed\x7fe\u0085f")
	got := out()
	i := strings.Index(got, ":msg=")
	if i < 0 {
		t.Fatalf("no msg in %q", got)
	}
	enc := strings.TrimSuffix(got[i+len(":msg="):], "\x1b\\")
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range string(dec) {
		if unicode.IsControl(c) {
			t.Errorf("decoded msg %q holds control rune %U", dec, c)
		}
	}
}

func TestMsgIsCapped(t *testing.T) {
	forceTTY(t, true)
	r, out := capture(t, nil)
	r.Done(strings.Repeat("x", 5000))
	got := out()
	if len(got) > 1024 {
		t.Errorf("sequence is %d bytes, want it capped well under the spec limit", len(got))
	}
}

// TestRootReportOnHerdrCoreHosts: in a herdr-core or herdr-app terminal
// (HC_TERMINAL_ID) the report is the terminal's root record, with no id, which
// is the only kind herdr-core turns into a status and herdr-app shows; in a
// herdr 0.9 pane (HERDR_ENV) and everywhere else it keeps the id'd record.
func TestRootReportOnHerdrCoreHosts(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		wantID bool
	}{
		{"herdr-core terminal", map[string]string{"HC_TERMINAL_ID": "t5b"}, false},
		{"herdr 0.9 pane", map[string]string{"HERDR_ENV": "1"}, true},
		{"herdr 0.9 pane with an hc variable", map[string]string{"HC_TERMINAL_ID": "t5b", "HERDR_ENV": "1"}, true},
		{"herdr 0.9 socket variable alone with an hc variable", map[string]string{"HC_TERMINAL_ID": "t5b", "HERDR_SOCKET_PATH": "/tmp/s"}, true},
		{"herdr 0.9 pane variable alone with an hc variable", map[string]string{"HC_TERMINAL_ID": "t5b", "HERDR_PANE_ID": "p1"}, true},
		{"inside tmux a stale hc variable keeps the id", map[string]string{"HC_TERMINAL_ID": "t5b", "TMUX": "/tmp/tmux-501/default,1,0"}, true},
		{"inside screen a stale hc variable keeps the id", map[string]string{"HC_TERMINAL_ID": "t5b", "STY": "123.pts-0.host"}, true},
		{"inside zellij a stale hc variable keeps the id", map[string]string{"HC_TERMINAL_ID": "t5b", "ZELLIJ": "0"}, true},
		{"any other host", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forceTTY(t, true)
			r, out := capture(t, tc.env)
			r.Blocked(KindPermission, "x")
			r.Clear()
			got := out()
			if hasID := strings.Contains(got, ":id=forgectl"); hasID != tc.wantID {
				t.Errorf("id present = %v, want %v in %q", hasID, tc.wantID, got)
			}
			if !strings.Contains(got, "state=blocked:") || !strings.Contains(got, ":app=forgectl") || !strings.Contains(got, "state=clear:") {
				t.Errorf("unexpected bytes %q", got)
			}
		})
	}
}

// TestRootReporterExactBytes pins the bytes: no id, same app, same msg.
func TestRootReporterExactBytes(t *testing.T) {
	var sb strings.Builder
	r := ForWriterRoot(&sb, false)
	r.Working("a", 5)
	r.Clear()
	want := "\x1b]7501;state=working:app=forgectl:progress=5:msg=" + b64("a") + "\x1b\\" +
		"\x1b]7501;state=clear:app=forgectl\x1b\\"
	if sb.String() != want {
		t.Errorf("got %q, want %q", sb.String(), want)
	}
}

// TestProgramIDIsTheSharedAppName: hcadapter's own-status check reads the same
// constant, so renaming the tool moves both sides together.
func TestProgramIDIsTheSharedAppName(t *testing.T) {
	if programID != meta.AppName {
		t.Errorf("programID = %q, want meta.AppName %q", programID, meta.AppName)
	}
}
