package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

// mailFakeAdapter fails with errs in order, then delivers, and keeps what it
// was handed.
type mailFakeAdapter struct {
	errs  []error
	texts []string
}

func (f *mailFakeAdapter) Deliver(_ context.Context, _ mail.Worker, _ mail.Message, text string) (string, error) {
	f.texts = append(f.texts, text)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return "", err
		}
	}
	return "delivered", nil
}

func (f *mailFakeAdapter) State(context.Context, mail.Worker) (mail.WorkerState, error) {
	return mail.StateUnknown, nil
}

// useMailLedger points the mail verbs at a temp ledger with a coordinator and
// three workers, every harness served by one fake adapter. The caller is the
// coordinator unless the test sets FORGECTL_WORKER.
func useMailLedger(t *testing.T) (*mail.Service, *mailFakeAdapter) {
	t.Helper()
	t.Setenv(mail.EnvWorker, "")
	dir := t.TempDir()
	roster := mail.FileRoster{Dir: dir}
	for _, w := range []mail.Worker{
		{Name: "coord", Harness: mail.HarnessClaude, Coordinator: true},
		{Name: "pi-1", Harness: mail.HarnessPi},
		{Name: "pi-2", Harness: mail.HarnessPi},
		{Name: "codex-1", Harness: mail.HarnessCodex},
	} {
		if err := roster.Put(w); err != nil {
			t.Fatal(err)
		}
	}
	ad := &mailFakeAdapter{}
	svc := &mail.Service{
		Box:    mail.Mailbox{Dir: dir},
		Roster: roster,
		Adapters: map[mail.Harness]mail.Adapter{
			mail.HarnessClaude: ad, mail.HarnessPi: ad, mail.HarnessCodex: ad,
		},
		Policy: mail.DefaultPolicy(),
	}
	prev := openMailSession
	openMailSession = func(_ module.Deps, getenv func(string) string) (*mailSession, error) {
		return &mailSession{svc: svc, ledger: dir, getenv: getenv}, nil
	}
	t.Cleanup(func() { openMailSession = prev })
	return svc, ad
}

// runSurface runs `forgectl surface <args>` and returns its streams and the
// exit code main would use.
func runSurface(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := newSurfaceCmd(module.Deps{})
	var out, errOut bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	code := 0
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		code = ExitCode(err)
		errOut.WriteString(err.Error())
	}
	return out.String(), errOut.String(), code
}

func TestSurfaceSendDelivers(t *testing.T) {
	_, ad := useMailLedger(t)
	out, errOut, code := runSurface(t, "", "send", "pi-1", "tests pass")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.HasPrefix(out, "sent ") || !strings.Contains(out, " to pi-1: delivered") {
		t.Fatalf("stdout %q", out)
	}
	if len(ad.texts) != 1 || !strings.HasPrefix(ad.texts[0], mail.Marker+" from=coord to=pi-1 ") {
		t.Fatalf("rendered %q", ad.texts)
	}
}

func TestSurfaceSendExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		worker string
		errs   []error
		args   []string
		want   int
		status string
	}{
		{"queued", "", []error{mail.NotReady("pi has not started")}, []string{"send", "pi-1", "hi", "--json"}, mailExitQueued, "queued"},
		{"failed", "", []error{errors.New("thread id is malformed")}, []string{"send", "codex-1", "hi", "--json"}, mailExitRefused, "failed"},
		{"worker to worker refused", "pi-1", nil, []string{"send", "pi-2", "hi"}, mailExitRefused, ""},
		{"unknown recipient", "", nil, []string{"send", "nobody", "hi"}, mailExitRefused, ""},
		{"bad priority is usage", "", nil, []string{"send", "pi-1", "hi", "--priority", "urgent"}, 1, ""},
		{"missing body is usage", "", nil, []string{"send", "pi-1"}, 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ad := useMailLedger(t)
			t.Setenv(mail.EnvWorker, tc.worker)
			ad.errs = tc.errs
			out, errOut, code := runSurface(t, "", tc.args...)
			if code != tc.want {
				t.Fatalf("exit %d, want %d; stdout %q stderr %q", code, tc.want, out, errOut)
			}
			if tc.status == "" {
				return
			}
			var res sendResult
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("stdout %q: %v", out, err)
			}
			if res.Status != tc.status || res.ID == "" {
				t.Fatalf("result %+v, want status %s", res, tc.status)
			}
		})
	}
}

func TestSurfaceSendBodySources(t *testing.T) {
	_, ad := useMailLedger(t)
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte("from a file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runSurface(t, "", "send", "pi-1", "@"+path); code != 0 {
		t.Fatalf("@file: exit %d, %s", code, errOut)
	}
	if _, errOut, code := runSurface(t, "from stdin\n", "send", "pi-2", "-"); code != 0 {
		t.Fatalf("stdin: exit %d, %s", code, errOut)
	}
	if len(ad.texts) != 2 || !strings.HasSuffix(ad.texts[0], "from a file\n") || !strings.HasSuffix(ad.texts[1], "from stdin\n") {
		t.Fatalf("delivered %q", ad.texts)
	}
	if _, _, code := runSurface(t, "", "send", "pi-1", "@"+filepath.Join(t.TempDir(), "absent")); code != mailExitRefused {
		t.Fatalf("missing @file: exit %d, want %d", code, mailExitRefused)
	}
}

// laterClock moves the service's clock past the retry backoff.
func laterClock(svc *mail.Service) {
	svc.Now = func() time.Time { return time.Now().Add(time.Minute) }
}

// send retries what is already queued before it sends, so the coordinator's
// own sends are the pump.
func TestSurfaceSendFlushesFirst(t *testing.T) {
	svc, ad := useMailLedger(t)
	ad.errs = []error{mail.NotReady("not started")}
	if _, _, code := runSurface(t, "", "send", "pi-1", "first"); code != mailExitQueued {
		t.Fatalf("first send exit %d, want queued", code)
	}
	laterClock(svc)
	if _, errOut, code := runSurface(t, "", "send", "pi-2", "second"); code != 0 {
		t.Fatalf("second send exit %d: %s", code, errOut)
	}
	entries, err := svc.Messages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Status != mail.StatusSent || entries[0].Attempts != 2 {
		t.Fatalf("entries %+v, want the queued one retried and sent", entries)
	}
	if len(ad.texts) != 3 || !strings.HasSuffix(ad.texts[1], "first") {
		t.Fatalf("delivery order %q, want the retry before the new send", ad.texts)
	}
}

func TestSurfaceWatchAndEvent(t *testing.T) {
	svc, ad := useMailLedger(t)
	if out, errOut, code := runSurface(t, "", "send", "pi-1", "review this", "--watch", "--json"); code != 0 || !strings.Contains(out, `"watching": true`) {
		t.Fatalf("send --watch: exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	t.Setenv(mail.EnvWorker, "pi-1")
	out, errOut, code := runSurface(t, "", "event", "--harness", "pi", "--state=idle", "--json")
	if code != 0 {
		t.Fatalf("event: exit %d, %s", code, errOut)
	}
	var res eventResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	if res.Worker != "pi-1" || res.State != "idle" || len(res.Notified) != 1 || res.Notified[0] != "coord" {
		t.Fatalf("event result %+v", res)
	}
	if last := ad.texts[len(ad.texts)-1]; !strings.Contains(last, "from=forgectl to=coord") || !strings.Contains(last, "pi-1 finished its turn") {
		t.Fatalf("idle notice %q", last)
	}
	w, err := svc.Roster.Get("pi-1")
	if err != nil {
		t.Fatal(err)
	}
	if w.State != mail.StateIdle || len(w.Watchers) != 0 {
		t.Fatalf("pi-1 after idle: %+v", w)
	}
}

func TestSurfaceEventHarnesses(t *testing.T) {
	svc, _ := useMailLedger(t)

	t.Setenv(mail.EnvWorker, "coord")
	out, errOut, code := runSurface(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s"}`, "event", "--harness", "claude")
	if code != 0 || out != "" {
		t.Fatalf("claude UserPromptSubmit: exit %d, stdout %q, stderr %q; want exit 0 and no stdout", code, out, errOut)
	}
	if w, _ := svc.Roster.Get("coord"); w.State != mail.StateBusy {
		t.Fatalf("coord state %q, want busy", w.State)
	}
	out, _, code = runSurface(t, `{"hook_event_name":"PreToolUse"}`, "event", "--harness", "claude")
	if code != 0 || out != "" {
		t.Fatalf("non-turn hook: exit %d, stdout %q; want a silent no-op", code, out)
	}
	if w, _ := svc.Roster.Get("coord"); w.State != mail.StateBusy {
		t.Fatalf("non-turn hook changed state to %q", w.State)
	}

	t.Setenv(mail.EnvWorker, "codex-1")
	notify := `{"type":"agent-turn-complete","thread-id":"b5f6c1c2-1111-2222-3333-444455556666","turn-id":"1","cwd":"/w"}`
	if _, errOut, code := runSurface(t, "", "event", "--harness", "codex", notify); code != 0 {
		t.Fatalf("codex notify: exit %d, %s", code, errOut)
	}
	if w, _ := svc.Roster.Get("codex-1"); w.ThreadID != "b5f6c1c2-1111-2222-3333-444455556666" || w.State != mail.StateIdle {
		t.Fatalf("codex-1 after notify: %+v", w)
	}
}

// A Claude Code hook that exits 2 blocks the stop or erases the prompt, so no
// event failure may exit 2.
func TestSurfaceEventNeverExitsTwo(t *testing.T) {
	useMailLedger(t)
	t.Setenv(mail.EnvWorker, "pi-1")
	for name, args := range map[string][]string{
		"bad JSON":           {"event", "--harness", "claude", "not json"},
		"unknown harness":    {"event", "--harness", "gemini"},
		"pi without state":   {"event", "--harness", "pi"},
		"state for claude":   {"event", "--harness", "claude", "--state", "idle", "{}"},
		"codex without JSON": {"event", "--harness", "codex"},
		"unknown worker":     {"event", "--harness", "pi", "--state", "idle"},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "unknown worker" {
				t.Setenv(mail.EnvWorker, "ghost")
			}
			_, _, code := runSurface(t, "", args...)
			if code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
		})
	}
}

func TestSurfaceInbox(t *testing.T) {
	useMailLedger(t)
	for _, args := range [][]string{{"send", "pi-1", "one"}, {"send", "pi-2", "two"}} {
		if _, errOut, code := runSurface(t, "", args...); code != 0 {
			t.Fatalf("%v: exit %d, %s", args, code, errOut)
		}
	}

	out, errOut, code := runSurface(t, "", "inbox", "pi-1", "--json")
	if code != 0 {
		t.Fatalf("inbox: exit %d, %s", code, errOut)
	}
	var rep inboxReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	if rep.Ledger == "" || rep.Name != "pi-1" || len(rep.Messages) != 1 || rep.Messages[0].Body != "one" || rep.Messages[0].Status != "sent" {
		t.Fatalf("inbox pi-1: %+v", rep)
	}

	out, _, code = runSurface(t, "", "inbox", "--all", "--json")
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 || len(rep.Messages) != 2 {
		t.Fatalf("inbox --all: exit %d, %+v, %v", code, rep, err)
	}
	out, _, code = runSurface(t, "", "inbox")
	if code != 0 || strings.Count(out, "\n") != 2 || !strings.Contains(out, "coord -> pi-2  two") {
		t.Fatalf("inbox (caller is coord): exit %d, stdout %q", code, out)
	}
	if _, _, code := runSurface(t, "", "inbox", "pi-1", "--all"); code != 1 {
		t.Fatalf("a name with --all: exit %d, want usage", code)
	}
}

func TestSurfaceFlush(t *testing.T) {
	svc, ad := useMailLedger(t)
	ad.errs = []error{mail.NotReady("not started")}
	if _, _, code := runSurface(t, "", "send", "pi-1", "hi"); code != mailExitQueued {
		t.Fatalf("send exit %d, want queued", code)
	}
	laterClock(svc)
	out, errOut, code := runSurface(t, "", "flush", "--json")
	if code != 0 {
		t.Fatalf("flush: exit %d, %s", code, errOut)
	}
	var rep flushReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	if rep.Sent != 1 {
		t.Fatalf("flush report %+v", rep)
	}
}

// Outside a coordinator session or a launched worker there is no ledger, and
// every mail verb says so instead of inventing one.
func TestDefaultMailSessionLedger(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := defaultMailSession(module.Deps{}, func(string) string { return "" }); !errors.Is(err, mail.ErrNoLedger) {
		t.Fatalf("no env: err = %v, want ErrNoLedger", err)
	}

	ledger := t.TempDir()
	runner := &exec.FakeRunner{}
	sess, err := defaultMailSession(module.Deps{Runner: runner}, func(k string) string {
		return map[string]string{mail.EnvLedger: ledger, "CLAUDE_CONFIG_DIR": "/cfg"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.ledger != ledger {
		t.Fatalf("ledger %q, want %q", sess.ledger, ledger)
	}
	if ca, ok := sess.svc.Adapters[mail.HarnessClaude].(mail.ClaudeAdapter); !ok || ca.ConfigDir != "/cfg" {
		t.Fatalf("claude adapter %#v", sess.svc.Adapters[mail.HarnessClaude])
	}
	if ca, ok := sess.svc.Adapters[mail.HarnessCodex].(mail.CodexAdapter); !ok || ca.Runner != runner {
		t.Fatalf("codex adapter %#v", sess.svc.Adapters[mail.HarnessCodex])
	}
}
