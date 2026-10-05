package ready

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture loads a captured screen and the herdr status read beside it. The
// screens were captured from live panes on herdr 0.9.1 (Claude Code 2.1.289,
// Codex 0.160.0) and sanitized of paths, hostnames, and session ids.
func fixture(t *testing.T, name string) Screen {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", name+".screen"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name+".status.json"))
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
		{"codex-ready", "codex", StateReady, "", "Ask Codex to do anything"},
		{"codex-typed-unsent", "codex", StateReady, "", "Run exactly this shell command once and nothing else: mkdir -p /tmp/fxcap/codex-made"},
		// The turn failed on a model error and Codex is back at its composer.
		{"codex-model-error", "codex", StateReady, "", "Ask Codex to do anything"},
		// A claude screen is never a ready codex, and the reverse.
		{"claude-ready-acceptedits", "codex", StateNotReady, "", ""},
		{"codex-ready", "claude", StateNotReady, "", ""},
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

func TestEvaluate_UnknownHarness(t *testing.T) {
	v := defaultTable(t).Evaluate("pi", fixture(t, "claude-ready-acceptedits"))
	if v.State != StateNotReady || !strings.Contains(v.Reason, `"pi"`) {
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
		"bad block":      "version = 1\n[[blocking]]\nname='x'\nany=['(']\n[harness.claude]\nagent='claude'\nprompt='x'\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); !errors.Is(err, ErrTable) {
				t.Fatalf("err = %v, want ErrTable", err)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	good := "version = 1\n[harness.claude]\nagent='claude'\nprompt='(?m)^READY$'\n"

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
		p := filepath.Join(dir, "loose.toml")
		if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o620); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); !errors.Is(err, ErrTable) {
			t.Fatalf("err = %v, want ErrTable", err)
		}
	})
}
