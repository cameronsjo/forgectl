package mail

import "testing"

func TestParseClaudeHook(t *testing.T) {
	cases := map[string]WorkerState{
		`{"hook_event_name":"Stop","session_id":"s","cwd":"/w"}`: StateIdle,
		`{"hook_event_name":"UserPromptSubmit"}`:                 StateBusy,
	}
	for in, want := range cases {
		got, err := ParseClaudeHook([]byte(in))
		if err != nil || got != want {
			t.Errorf("ParseClaudeHook(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`{"hook_event_name":"PreToolUse"}`, `not json`} {
		if _, err := ParseClaudeHook([]byte(bad)); err == nil {
			t.Errorf("ParseClaudeHook(%s) = nil error", bad)
		}
	}
}

func TestParseCodexNotify(t *testing.T) {
	for _, in := range []string{
		`{"type":"agent-turn-complete","thread-id":"th_1","turn-id":"t"}`,
		`{"type":"agent-turn-complete","thread_id":"th_1"}`,
		`{"type":"agent-turn-complete","threadId":"th_1"}`,
	} {
		st, thread, err := ParseCodexNotify([]byte(in))
		if err != nil || st != StateIdle || thread != "th_1" {
			t.Errorf("ParseCodexNotify(%s) = %q, %q, %v", in, st, thread, err)
		}
	}
	st, thread, err := ParseCodexNotify([]byte(`{"type":"agent-turn-complete"}`))
	if err != nil || st != StateIdle || thread != "" {
		t.Errorf("no thread id: %q, %q, %v", st, thread, err)
	}
	if _, _, err := ParseCodexNotify([]byte(`{"type":"approval-requested"}`)); err == nil {
		t.Error("accepted a non-turn notify")
	}
}

func TestParseState(t *testing.T) {
	for _, ok := range []string{"idle", "busy", "waiting"} {
		if _, err := ParseState(ok); err != nil {
			t.Errorf("ParseState(%q) = %v", ok, err)
		}
	}
	if _, err := ParseState("asleep"); err == nil {
		t.Error("ParseState(asleep) = nil error")
	}
}
