package resume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Screens are built from the layout measured on Claude Code 2.1.283/2.1.285
// in a herdr pane: output, an optional notice line, a rule, the "❯" input
// line, a rule, then status lines.
const testRule = "──────────────────────────────────────────────────────────────────────────"

func screen(box ...string) string {
	lines := []string{
		"❯ reply ok",
		"⏺ Standing by.",
		"",
		"                                            ✔ Update installed · Restart to update",
		testRule,
	}
	lines = append(lines, box...)
	lines = append(lines, testRule,
		"  Haiku 4.5 ⎇ no git  · $0.11 · <1m · sjomba",
		"  /private/tmp/claude-probe/e2e",
	)
	return strings.Join(lines, "\n")
}

func TestInputLineEmpty(t *testing.T) {
	tests := []struct {
		name      string
		screen    string
		wantEmpty bool
		reason    string
	}{
		{"empty prompt", screen("❯"), true, ""},
		{"empty prompt with trailing spaces", screen("❯      "), true, ""},
		{"empty prompt with a non-breaking space", screen("❯ "), true, ""},
		{"CRLF line endings", strings.ReplaceAll(screen("❯"), "\n", "\r\n"), true, ""},
		{"draft", screen("❯ unsent draft text"), false, reasonDraft},
		{"multi-line draft", screen("❯ first line", "  second line"), false, reasonDraft},
		{"draft only on a continuation line", screen("❯", "  pasted text"), false, reasonDraft},
		{"box line that does not start with the prompt", screen("  some text"), false, reasonBoxUnrecognized},
		{"empty box", testRule + "\n" + testRule, false, reasonBoxUnrecognized},
		{"one rule only", "output\n" + testRule + "\n❯\nstatus", false, reasonBoxUnrecognized},
		{"no rules: a bare shell prompt", "/private/tmp/claude-probe/e2e\n❯", false, reasonBoxUnrecognized},
		{"blank screen", "", false, reasonBoxUnrecognized},
		{"short dash runs are not rules", "────\n❯\n────", false, reasonBoxUnrecognized},
		{
			"bypass-permissions footer",
			screen("❯") + "\n  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			true, "",
		},
		{
			"bypass-permissions footer with a draft",
			screen("❯ rm -rf build") + "\n  ⏵⏵ bypass permissions on (shift+tab to cycle) · 1 shell",
			false, reasonDraft,
		},
		{
			"plan-mode footer",
			screen("❯") + "\n  ⏸ plan mode on (shift+tab to cycle)",
			true, "",
		},
		{
			"rule carrying a short label",
			strings.Replace(screen("❯"), testRule, "──────────────── my-session ────────────────────────────────────────────────", 1),
			true, "",
		},
		{
			// A footer drawing its own rule shifts the "last two rules" onto the
			// status lines, which do not start with the prompt: not recognized,
			// never a false "empty".
			"footer that draws a rule",
			screen("❯ draft") + "\n" + testRule,
			false, reasonBoxUnrecognized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			empty, reason := InputLineEmpty(tt.screen)
			if empty != tt.wantEmpty || reason != tt.reason {
				t.Errorf("InputLineEmpty = (%v, %q), want (%v, %q)", empty, reason, tt.wantEmpty, tt.reason)
			}
		})
	}
}

func TestIsClaudeExec(t *testing.T) {
	for path, want := range map[string]bool{
		"/Users/u/.local/bin/claude":                   true,
		"/Users/u/.local/share/claude/versions/2.1.283": true,
		"/opt/homebrew/bin/claude":                     true,
		"/usr/local/bin/node":                          false,
		"/Users/u/.local/share/claude/versions/latest": false,
		"/tmp/2.1.283":                                 false,
		"/usr/bin/claude-code":                         false,
		"":                                             false,
	} {
		if got := IsClaudeExec(path); got != want {
			t.Errorf("IsClaudeExec(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseProcStart(t *testing.T) {
	got, err := ParseProcStart("Tue Sep 29 23:12:27 2026")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 29, 23, 12, 27, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %v, want %v (procStart is UTC)", got, want)
	}
	padded, err := ParseProcStart("Thu Sep  3 01:02:03 2026")
	if err != nil {
		t.Fatalf("space-padded day: %v", err)
	}
	if padded.Day() != 3 {
		t.Errorf("padded day = %d", padded.Day())
	}
	for _, bad := range []string{"", "yesterday", "2026-09-29T23:12:27Z"} {
		if _, err := ParseProcStart(bad); err == nil {
			t.Errorf("ParseProcStart(%q) accepted", bad)
		}
	}
}

const (
	testSID       = "432d45ef-ef70-4f06-967b-ee452b93abb9"
	testPid       = 90103
	testPane      = "w7P:p1"
	testProcStart = "Wed Sep 30 01:41:04 2026"
)

func testSession() OutdatedSession {
	return OutdatedSession{
		SessionID: testSID, Pid: testPid, Status: "idle", Version: "2.1.283",
		InstalledVersion: "2.1.285", Pane: testPane, ProcStart: testProcStart,
	}
}

// readyObservation is an Observation every check passes; each test case
// breaks exactly one thing.
func readyObservation() Observation {
	start, _ := ParseProcStart(testProcStart)
	return Observation{
		Entry:      RegistryEntry{Pid: testPid, SessionID: testSID, Status: "idle", ProcStart: testProcStart},
		EntryFound: true,
		Alive:      true,
		Proc:       ProcIdentity{ExecPath: "/Users/u/.local/share/claude/versions/2.1.283", Start: start.Add(400 * time.Millisecond)},
		Pane:       PaneState{Agent: "claude", AgentSession: testSID, ForegroundPGID: testPid, ShellPID: 51365, ForegroundPIDs: []int{testPid}},
		Screen:     screen("❯"),
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OutdatedSession, *Observation)
		want   Readiness
		reason string // substring
	}{
		{"all checks pass", func(*OutdatedSession, *Observation) {}, Ready, "input line empty"},

		// Check 1: identity. Every failure refuses.
		{"process exited", func(_ *OutdatedSession, o *Observation) { o.Alive = false }, Refused, "has exited"},
		{"registry file gone", func(_ *OutdatedSession, o *Observation) { o.EntryFound = false }, Refused, "is gone"},
		{"pid reused by another session", func(_ *OutdatedSession, o *Observation) { o.Entry.SessionID = "ffff0000" }, Refused, "another session"},
		{"no procStart recorded", func(s *OutdatedSession, o *Observation) { s.ProcStart, o.Entry.ProcStart = "", "" }, Refused, "no procStart"},
		{"procStart changed", func(_ *OutdatedSession, o *Observation) { o.Entry.ProcStart = "Wed Sep 30 02:00:00 2026" }, Refused, "start time changed"},
		{"identity unreadable", func(_ *OutdatedSession, o *Observation) { o.ProcErr = errors.New("EPERM") }, Refused, "could not read"},
		{"pid reused by a non-claude process", func(_ *OutdatedSession, o *Observation) { o.Proc.ExecPath = "/bin/sleep" }, Refused, "not running a claude binary"},
		{"pid reused by a claude that has not written its file yet", func(_ *OutdatedSession, o *Observation) { o.Proc.Start = o.Proc.Start.Add(time.Minute) }, Refused, "different time"},
		{"procStart unparseable", func(s *OutdatedSession, o *Observation) { s.ProcStart, o.Entry.ProcStart = "garbage", "garbage" }, Refused, "does not parse"},

		// Check 2: status. Anything but idle waits.
		{"busy", func(_ *OutdatedSession, o *Observation) { o.Entry.Status = "busy" }, NotYet, `"busy"`},
		{"background shell", func(_ *OutdatedSession, o *Observation) { o.Entry.Status = "shell" }, NotYet, `"shell"`},
		{"permission prompt", func(_ *OutdatedSession, o *Observation) { o.Entry.Status = "waiting" }, NotYet, `"waiting"`},

		// Check 3: the pane. Every failure refuses.
		{"pane vanished", func(_ *OutdatedSession, o *Observation) { o.PaneErr = errors.New("pane_not_found") }, Refused, "cannot show pane"},
		{"pane labelled with another session", func(_ *OutdatedSession, o *Observation) { o.Pane.AgentSession = "ffff0000" }, Refused, "different session"},
		{"pane runs another agent", func(_ *OutdatedSession, o *Observation) { o.Pane.Agent = "codex" }, Refused, "different session"},
		{"pid not the pane's foreground (nested)", func(_ *OutdatedSession, o *Observation) { o.Pane.ForegroundPIDs = []int{12345} }, Refused, "not pane"},

		// Check 4: the input line. Waits.
		{"draft", func(_ *OutdatedSession, o *Observation) { o.Screen = screen("❯ half a thought") }, NotYet, "draft"},
		{"unrecognized screen", func(_ *OutdatedSession, o *Observation) { o.Screen = "$ " }, NotYet, "not recognized"},
		{"screen unreadable", func(_ *OutdatedSession, o *Observation) { o.ScreenErr = errors.New("x") }, NotYet, "could not read the pane's screen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, o := testSession(), readyObservation()
			tt.mutate(&s, &o)
			got := Evaluate(s, o)
			if got.Readiness != tt.want || !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("Evaluate = %+v, want readiness %d with reason containing %q", got, tt.want, tt.reason)
			}
		})
	}
}

func TestEvaluate_IdentityOutranksBusy(t *testing.T) {
	s, o := testSession(), readyObservation()
	o.Entry.Status = "busy"
	o.Proc.ExecPath = "/bin/sleep"
	if got := Evaluate(s, o); got.Readiness != Refused {
		t.Fatalf("a busy session whose pid is not claude = %+v; want refused, not queued", got)
	}
}

func TestPlanRestart(t *testing.T) {
	idle := testSession()
	busy := testSession()
	busy.SessionID, busy.Status, busy.Busy = "bbbb", "busy", true
	nopane := testSession()
	nopane.SessionID, nopane.Pane = "cccc", ""
	odd := testSession()
	odd.SessionID, odd.VersionUnparseable = "dddd", true
	list := []OutdatedSession{idle, busy, nopane, odd}

	plan := PlanRestart(list, nil)
	want := map[string]RestartAction{testSID: ActionRestart, "bbbb": ActionRestart, "cccc": ActionManual, "dddd": ActionSkip}
	if len(plan) != len(want) {
		t.Fatalf("plan has %d items, want %d", len(plan), len(want))
	}
	for _, item := range plan {
		if item.Action != want[item.SessionID] {
			t.Errorf("%s: action %d, want %d (%s)", item.SessionID, item.Action, want[item.SessionID], item.Reason)
		}
	}
	if !strings.Contains(plan[2].Reason, ManualResume("cccc")) {
		t.Errorf("a pane-less session's reason must carry the by-hand command: %q", plan[2].Reason)
	}

	only := PlanRestart(list, []string{"bbbb", "eeee"})
	if len(only) != 2 || only[0].SessionID != "bbbb" || only[1].SessionID != "eeee" || only[1].Action != ActionSkip {
		t.Fatalf("--session plan = %+v; want bbbb, then eeee reported as a skip", only)
	}
}

func TestParseHerdrReplies(t *testing.T) {
	// Captured from herdr 0.9.1.
	get := `{"id":"cli:pane:get","result":{"pane":{"agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"432d45ef-ef70-4f06-967b-ee452b93abb9"},"agent_status":"unknown","pane_id":"w7P:p1"},"type":"pane_info"}}`
	pi := `{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":90103,"foreground_processes":[{"argv":["2.1.283","--model","haiku"],"argv0":"2.1.283","name":"2.1.283","pid":90103}],"pane_id":"w7P:p1","shell_pid":51365},"type":"pane_process_info"}}`
	st, err := parsePaneGet(get)
	if err != nil {
		t.Fatal(err)
	}
	if err := parseProcessInfo(pi, &st); err != nil {
		t.Fatal(err)
	}
	if st.Agent != "claude" || st.AgentSession != testSID || st.ForegroundPGID != 90103 || st.ShellPID != 51365 || len(st.ForegroundPIDs) != 1 || st.ForegroundPIDs[0] != 90103 {
		t.Fatalf("parsed %+v", st)
	}
	if st.ShellForeground() {
		t.Error("claude holds the foreground, so the shell does not")
	}
	st.ForegroundPGID = 51365
	if !st.ShellForeground() {
		t.Error("the shell's own group in the foreground is the ready state")
	}

	unlabelled, err := parsePaneGet(`{"result":{"pane":{"pane_id":"w7P:p1"}}}`)
	if err != nil || unlabelled.AgentSession != "" {
		t.Errorf("a pane with no agent_session = %+v, %v; want an empty label", unlabelled, err)
	}
	for _, bad := range []string{"", "not json", `{"result":{}}`} {
		if _, err := parsePaneGet(bad); err == nil {
			t.Errorf("parsePaneGet(%q) accepted", bad)
		}
		if err := parseProcessInfo(bad, &PaneState{}); err == nil {
			t.Errorf("parseProcessInfo(%q) accepted", bad)
		}
	}
	if (PaneState{}).ShellForeground() {
		t.Error("an unknown shell pid is never ready")
	}
}

func TestReadEntryAndLiveSession(t *testing.T) {
	root := t.TempDir()
	p := Paths{ClaudeHome: root}
	dir := p.registryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("100.json", `{"pid":100,"sessionId":"abcd","status":"idle","procStart":"`+testProcStart+`"}`)
	write("200.json", `{"pid":200,"sessionId":"-c","status":"idle"}`)
	prev := pidAlive
	pidAlive = func(pid int) bool { return pid == 100 }
	t.Cleanup(func() { pidAlive = prev })

	e, ok := ReadEntry(p, 100)
	if !ok || e.SessionID != "abcd" || e.ProcStart != testProcStart || !e.Live {
		t.Fatalf("ReadEntry(100) = %+v, %v", e, ok)
	}
	for _, pid := range []int{200, 300, 0, -1} {
		if _, ok := ReadEntry(p, pid); ok {
			t.Errorf("ReadEntry(%d) found an entry (invalid id, missing file, or bad pid)", pid)
		}
	}
	if _, ok := LiveSession(p, "abcd"); !ok {
		t.Error("LiveSession missed a live session")
	}
	pidAlive = func(int) bool { return false }
	if _, ok := LiveSession(p, "abcd"); ok {
		t.Error("LiveSession returned a dead session")
	}
}

// TestReadProcessIdentity_ReadsThisProcess exercises the real platform reader
// against the test binary itself.
func TestReadProcessIdentity_ReadsThisProcess(t *testing.T) {
	id, err := readProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("platform cannot read process identity here: %v", err)
	}
	if !filepath.IsAbs(id.ExecPath) || !strings.HasSuffix(id.ExecPath, filepath.Base(os.Args[0])) {
		t.Errorf("exec path = %q, want this test binary (%s)", id.ExecPath, os.Args[0])
	}
	if age := time.Since(id.Start); age < 0 || age > time.Hour {
		t.Errorf("start time %v is %v ago; want within this test run", id.Start, age)
	}
}
