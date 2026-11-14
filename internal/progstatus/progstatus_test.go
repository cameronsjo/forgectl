package progstatus

import (
	"encoding/base64"
	"io"
	"os"
	"strings"
	"testing"
	"unicode"
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
		{"blocked auth", func(r *Reporter) { r.Blocked("auth", "Touch ID") }, pre + "state=blocked" + idApp + ":kind=auth:msg=" + b64("Touch ID") + "\x1b\\"},
		{"blocked question", func(r *Reporter) { r.Blocked("question", "ok?") }, pre + "state=blocked" + idApp + ":msg=" + b64("ok?") + "\x1b\\"},
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
	if got := out(); !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Errorf("tmux output = %q, want ESC P tmux; prefix", got)
	}
}

func TestHostileMsgHasNoControlRunes(t *testing.T) {
	forceTTY(t, true)
	r, out := capture(t, nil)
	r.Error("a\x1b]0;pwn\x07b\nc‮d\x7fe\u0085f")
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
