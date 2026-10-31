package hcadapter

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestSendLineTypesOnlyIntoAnAgentAtItsPrompt pins SendLine's read before it
// types: hc presses Enter after the line, so a terminal that is gone, has
// exited, runs no agent harness (a shell, an editor, a REPL), or is mid-turn
// gets nothing.
func TestSendLineTypesOnlyIntoAnAgentAtItsPrompt(t *testing.T) {
	agent := func(state string) string { return `{"app":"claude-code","state":"` + state + `"}` }
	cases := []struct {
		name   string
		reply  string // "" means no terminal at the address
		sent   bool
		ifIdle bool
	}{
		{"idle agent", getReply("t9", "", 100, "live", agent("idle")), true, true},
		{"done agent", getReply("t9", "", 100, "live", agent("done")), true, false},
		{"working agent", getReply("t9", "", 100, "live", agent("working")), false, false},
		{"blocked agent", getReply("t9", "", 100, "live", agent("blocked")), false, false},
		{"plain shell", getReply("t9", "", 100, "live", `null`), false, false},
		{"no app", getReply("t9", "", 100, "live", `{"state":"idle"}`), false, false},
		{"exited", getReply("t9", "", 100, "finished", agent("done")), false, false},
		{"absent", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
			if tc.reply != "" {
				f.terminals["t9"] = tc.reply
			}
			run := f.runner()
			a, err := New(run, "/usr/local/bin/hc", func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			err = a.SendLine(context.Background(), "t9", "DESK-RESPONSE ask-1 done")
			sent := false
			for _, v := range f.verbs {
				if v == "send" {
					sent = true
				}
			}
			if sent != tc.sent {
				t.Fatalf("sent = %v, want %v (err %v)", sent, tc.sent, err)
			}
			if !tc.sent {
				if !errors.Is(err, ErrSendRefused) {
					t.Fatalf("err = %v, want ErrSendRefused", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			last, _ := run.Last()
			has := false
			for i := 0; i+1 < len(last.Args); i++ {
				if last.Args[i].Equal(exec.MustFixed("--if")) && last.Args[i+1].Equal(exec.MustFixed("idle")) {
					has = true
				}
			}
			if has != tc.ifIdle {
				t.Errorf("--if idle = %v, want %v", has, tc.ifIdle)
			}
		})
	}
}

func TestSendLineRefusesHostileArguments(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a, err := New(f.runner(), "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ addr, text string }{
		{"-x", "ok"}, {"t9 t1", "ok"}, {"", "ok"},
		{"t9", "two\nlines"}, {"t9", "esc \x1b[2J"}, {"t9", "bidi \u202e"},
	} {
		if err := a.SendLine(context.Background(), tc.addr, tc.text); !errors.Is(err, ErrSendRefused) {
			t.Errorf("SendLine(%q, %q) = %v, want ErrSendRefused", tc.addr, tc.text, err)
		}
	}
	if len(f.verbs) != 0 {
		t.Fatalf("hc was called for a refused argument: %v", f.verbs)
	}
}
