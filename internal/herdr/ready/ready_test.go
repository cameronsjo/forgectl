package ready

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixture loads a captured screen and the herdr status read beside it. The
// screens were captured from live panes on herdr 0.9.1 (Claude Code 2.1.289,
// Codex 0.160.0) and herdr 0.9.3 (Pi 1.0.4), and sanitized of paths,
// hostnames, and session ids.
func fixture(t *testing.T, name string) Screen {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", name+".screen")) //nolint:gosec // G304: name is a literal at every call site
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name+".status.json")) //nolint:gosec // G304: name is a literal at every call site
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Agent  *string `json:"agent"`
		Status string  `json:"agent_status"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	s := Screen{Text: strings.TrimRight(string(text), "\n"), Status: st.Status}
	if st.Agent != nil {
		s.Agent = *st.Agent
	}
	return s
}

func defaultTable(t *testing.T) *Table {
	t.Helper()
	tab, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	return tab
}

// TestEvaluate_CapturedScreens pins every captured screen to its verdict:
// each ready screen passes, and each blocking screen fails naming itself.
func TestEvaluate_CapturedScreens(t *testing.T) {
	tab := defaultTable(t)
	cases := []struct {
		fixture  string
		harness  string
		state    State
		blocking string
		input    string
	}{
		{"claude-ready-acceptedits", "claude", StateReady, "", ""},
		{"claude-ready-plan", "claude", StateReady, "", ""},
		{"claude-typed-unsent", "claude", StateReady, "", "Run the shell command `date -u` once. Do nothing else."},
		{"claude-permission-prompt", "claude", StateBlocked, "permission prompt", ""},
		{"claude-plan-approval", "claude", StateBlocked, "plan-approval dialog", ""},
		{"claude-trust-dialog", "claude", StateBlocked, "folder-trust dialog", ""},
		{"npm-ok-to-proceed", "claude", StateBlocked, "npm install prompt", ""},
		{"npm-ok-to-proceed", "codex", StateBlocked, "npm install prompt", ""},
		{"shell-idle", "claude", StateNotReady, "", ""},
		{"shell-idle", "codex", StateNotReady, "", ""},
		// The placeholder in an empty composer is not input.
		{"codex-ready", "codex", StateReady, "", ""},
		{"codex-typed-unsent", "codex", StateReady, "", "Run exactly this shell command once and nothing else: mkdir -p /tmp/fxcap/codex-made"},
		// The turn failed on a model error and Codex is back at its composer.
		{"codex-model-error", "codex", StateReady, "", ""},
		// Input that wraps inside the box is joined back into one line.
		{"claude-typed-wrapped", "claude", StateReady, "", "1. Read the README first. This line is deliberately long so that it wraps inside the input box of the terminal user interface and continues onto a second visual row, and then onto a third row as well to be sure."},
		{"codex-typed-wrapped", "codex", StateReady, "", "This codex composer line is deliberately long so that it wraps onto a second visual row inside the composer area of the codex terminal interface, and then a third. Adding more words here so that the composer definitely has to wrap to another row now."},
		// A past message starting "1." is transcript, not a menu: the input
		// box at the bottom of the screen wins for claude.
		{"claude-idle-numbered-history", "claude", StateReady, "", ""},
		// A claude screen is never a ready codex, and the reverse.
		{"claude-ready-acceptedits", "codex", StateNotReady, "", ""},
		{"codex-ready", "claude", StateNotReady, "", ""},
		// Pi 1.0.4: an empty editor, after a turn (herdr says done), typed
		// input, and input that wraps (continuation rows are not indented).
		{"pi-ready", "pi", StateReady, "", ""},
		{"pi-ready-after-turn", "pi", StateReady, "", ""},
		{"pi-typed-unsent", "pi", StateReady, "", "Run the shell command date -u once. Do nothing else."},
		{"pi-typed-wrapped", "pi", StateReady, "", "Run the shell command date -u once. Do nothing else. This pi editor line is deliberately long so that it wraps onto a second visual row inside the editor area of the pi terminal interface, and then a third row as well. Adding more words here so that the editor definitely has to wrap to another row now, and keep going a little longer to be very sure of it."},
		// Pi keeps its editor on screen while a turn runs; herdr's working
		// status is what keeps it not ready.
		{"pi-working", "pi", StateNotReady, "", ""},
		{"pi-trust-dialog", "pi", StateBlocked, "project trust dialog", ""},
		{"npm-ok-to-proceed", "pi", StateBlocked, "npm install prompt", ""},
		{"shell-idle", "pi", StateNotReady, "", ""},
		// Another harness's screen is never a ready pi, and the reverse.
		{"claude-ready-acceptedits", "pi", StateNotReady, "", ""},
		{"codex-ready", "pi", StateNotReady, "", ""},
		{"pi-ready", "claude", StateNotReady, "", ""},
		{"pi-ready", "codex", StateNotReady, "", ""},
	}
	for _, c := range cases {
		t.Run(c.fixture+"/"+c.harness, func(t *testing.T) {
			v := tab.Evaluate(c.harness, fixture(t, c.fixture))
			if v.State != c.state || v.Blocking != c.blocking {
				t.Fatalf("verdict %+v, want state %q blocking %q", v, c.state, c.blocking)
			}
			if c.state == StateReady && v.Input != c.input {
				t.Errorf("input %q, want %q", v.Input, c.input)
			}
			if c.state != StateReady && v.Reason == "" {
				t.Errorf("a refusal must say why: %+v", v)
			}
		})
	}
}

// TestEvaluate_SignalsMustAgree: a visible prompt is not enough. Each
// disagreeing herdr signal keeps the worker not ready, and says which.
func TestEvaluate_SignalsMustAgree(t *testing.T) {
	tab := defaultTable(t)
	base := fixture(t, "claude-ready-acceptedits")
	for name, mutate := range map[string]func(*Screen){
		"herdr reports blocked": func(s *Screen) { s.Status = "blocked" },
		"herdr reports working": func(s *Screen) { s.Status = "working" },
		"herdr reports unknown": func(s *Screen) { s.Status = "unknown" },
		"herdr sees no agent":   func(s *Screen) { s.Agent = "" },
		"herdr sees codex":      func(s *Screen) { s.Agent = "codex" },
	} {
		t.Run(name, func(t *testing.T) {
			s := base
			mutate(&s)
			v := tab.Evaluate("claude", s)
			if v.State != StateNotReady || v.Reason == "" {
				t.Fatalf("verdict %+v, want not-ready with a reason", v)
			}
		})
	}
}

// TestEvaluate_DialogOverPromptIsBlocked: a dialog that leaves the input
// box's rule lines on screen is still a dialog. Blocking wins.
func TestEvaluate_DialogOverPromptIsBlocked(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "claude-ready-acceptedits")
	s.Text += "\n Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	if v := tab.Evaluate("claude", s); v.State != StateBlocked {
		t.Fatalf("verdict %+v, want blocked", v)
	}
}

// TestEvaluate_TranscriptAboveThePromptIsNotADialog: a claude reply that
// mentions a dialog's wording sits above the input box. The box at the
// bottom wins; the words are transcript.
func TestEvaluate_TranscriptAboveThePromptIsNotADialog(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "claude-idle-numbered-history")
	box := strings.Index(s.Text, "\n─")
	s.Text = s.Text[:box] + "\n  Do you want to proceed with option A? [y/N]\n  Esc to cancel · Tab to amend" + s.Text[box:]
	if v := tab.Evaluate("claude", s); !v.Ready() {
		t.Fatalf("verdict %+v, want ready", v)
	}
}

// TestEvaluate_OverlayWithTheBoxVisibleIsBlocked: a with_prompt row applies
// even when a prompt_first harness shows its input box, for an overlay drawn
// above the box (Claude's session feedback survey; a digit typed into the
// box answers it).
func TestEvaluate_OverlayWithTheBoxVisibleIsBlocked(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "claude-ready-acceptedits")
	box := strings.Index(s.Text, "\n─")
	s.Text = s.Text[:box] + "\n● How is Claude doing this session? (optional)\n  1: Bad    2: Fine   3: Good   0: Dismiss" + s.Text[box:]
	v := tab.Evaluate("claude", s)
	if v.State != StateBlocked || v.Blocking != "session feedback survey" {
		t.Fatalf("verdict %+v, want blocked by the survey", v)
	}
}

// TestEvaluate_DialogRowInTheFooterIsBlocked: every blocking row is checked
// against the rows under the box, where no dialog text belongs.
func TestEvaluate_DialogRowInTheFooterIsBlocked(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "claude-ready-acceptedits")
	s.Text += "\n  Esc to cancel · Tab to amend"
	if v := tab.Evaluate("claude", s); v.State != StateBlocked {
		t.Fatalf("verdict %+v, want blocked", v)
	}
}

// TestEvaluate_TextBelowThePromptIsNotFooter: the box must sit at the bottom
// with only footer rows under it. Unindented text below means something is
// drawn under the box, so the box is not the live input.
func TestEvaluate_TextBelowThePromptIsNotFooter(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "claude-ready-acceptedits")
	s.Text += "\nsomething drawn below the input box"
	if v := tab.Evaluate("claude", s); v.Ready() {
		t.Fatalf("verdict %+v, want not ready", v)
	}
}

// TestEvaluate_ShellPromptIsNotClaude: starship draws the same `❯` glyph
// claude uses. Without the input box's rule lines it is not a claude prompt,
// even if herdr were to misreport the agent.
func TestEvaluate_ShellPromptIsNotClaude(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "shell-idle")
	s.Agent, s.Status = "claude", "idle"
	if v := tab.Evaluate("claude", s); v.State != StateNotReady {
		t.Fatalf("verdict %+v, want not-ready", v)
	}
}

// TestEvaluate_PiTranscriptIsNotADialog: pi draws transcript rows indented
// one space, as it draws its dialogs. With the editor at the bottom, a reply
// that quotes a dialog's wording is transcript.
func TestEvaluate_PiTranscriptIsNotADialog(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "pi-ready-after-turn")
	box := strings.LastIndex(s.Text, "\n─")
	box = strings.LastIndex(s.Text[:box], "\n─")
	s.Text = s.Text[:box] + "\n Trust project folder?\n → Trust\n ↑↓ navigate  enter select  escape/ctrl+c cancel" + s.Text[box:]
	if v := tab.Evaluate("pi", s); !v.Ready() {
		t.Fatalf("verdict %+v, want ready", v)
	}
}

// TestEvaluate_PiDialogRowInTheFooterIsBlocked: a selector footer under the
// editor is checked, as for claude.
func TestEvaluate_PiDialogRowInTheFooterIsBlocked(t *testing.T) {
	tab := defaultTable(t)
	s := fixture(t, "pi-ready")
	s.Text += "\n ↑↓ navigate  enter select  escape/ctrl+c cancel"
	if v := tab.Evaluate("pi", s); v.State != StateBlocked {
		t.Fatalf("verdict %+v, want blocked", v)
	}
}

func TestEvaluate_UnknownHarness(t *testing.T) {
	v := defaultTable(t).Evaluate("gemini", fixture(t, "claude-ready-acceptedits"))
	if v.State != StateNotReady || !strings.Contains(v.Reason, `"gemini"`) {
		t.Fatalf("verdict %+v, want not-ready naming the harness", v)
	}
}

func TestParse_Refusals(t *testing.T) {
	for name, src := range map[string]string{
		"wrong version":  "version = 2\n[harness.claude]\nagent='claude'\nprompt='x'\n",
		"unknown key":    "version = 1\nsurprise = true\n[harness.claude]\nagent='claude'\nprompt='x'\n",
		"no harness":     "version = 1\n",
		"no agent":       "version = 1\n[harness.claude]\nprompt='x'\n",
		"no prompt":      "version = 1\n[harness.claude]\nagent='claude'\n",
		"bad prompt":     "version = 1\n[harness.claude]\nagent='claude'\nprompt='('\n",
		"nameless block": "version = 1\n[[blocking]]\nany=['x']\n[harness.claude]\nagent='claude'\nprompt='x'\n",
		"empty block":    "version = 1\n[[blocking]]\nname='x'\n[harness.claude]\nagent='claude'\nprompt='x'\n",
		"bad block":      "version = 1\n[[blocking]]\nname='x'\nany=['(']\n[harness.claude]\nagent='claude'\nprompt='x(?P<footer>)'\n",
		"no footer":      "version = 1\n[harness.claude]\nagent='claude'\nprompt='x'\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); !errors.Is(err, ErrTable) {
				t.Fatalf("err = %v, want ErrTable", err)
			}
		})
	}
}

// skipWithoutOverride skips where the override is refused outright: it needs
// O_NOFOLLOW and an owner check (table_other.go).
func skipWithoutOverride(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("predicate overrides are unix-only")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	good := "version = 1\n[harness.claude]\nagent='claude'\nprompt='(?m)^READY(?P<footer>)$'\n"

	t.Run("missing file uses the built-in table", func(t *testing.T) {
		tab, err := Load(filepath.Join(dir, "absent.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if v := tab.Evaluate("codex", fixture(t, "codex-ready")); !v.Ready() {
			t.Fatalf("built-in table lost codex: %+v", v)
		}
	})

	t.Run("override replaces the table whole", func(t *testing.T) {
		skipWithoutOverride(t)
		p := filepath.Join(dir, "override.toml")
		if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
			t.Fatal(err)
		}
		tab, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if v := tab.Evaluate("codex", fixture(t, "codex-ready")); v.Ready() {
			t.Fatal("an override without codex still knew codex")
		}
		if v := tab.Evaluate("claude", Screen{Text: "READY", Agent: "claude", Status: "idle"}); !v.Ready() {
			t.Fatalf("override not applied: %+v", v)
		}
	})

	t.Run("symlink refused", func(t *testing.T) {
		skipWithoutOverride(t)
		target := filepath.Join(dir, "target.toml")
		if err := os.WriteFile(target, []byte(good), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.toml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(link); !errors.Is(err, ErrTable) {
			t.Fatalf("err = %v, want ErrTable", err)
		}
	})

	t.Run("group-writable refused", func(t *testing.T) {
		skipWithoutOverride(t)
		p := filepath.Join(dir, "loose.toml")
		if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o620); err != nil { //nolint:gosec // G302: the test needs a group-writable file to prove Load refuses it
			t.Fatal(err)
		}
		if _, err := Load(p); !errors.Is(err, ErrTable) {
			t.Fatalf("err = %v, want ErrTable", err)
		}
	})
}
