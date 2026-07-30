package ghostty

// Test plan for ghostty.go
//
// New / resolveBin (Classification: ops layer)
//   [x] Happy: a PATH hit is used as the resolved binary
//   [x] Happy: a PATH miss falls back to the cmux-bundled path when it exists
//   [x] Unhappy: neither PATH nor the fallback resolves — bin stays ""
//   [x] Happy: WithBin skips resolution entirely
//
// Client.Themes (Classification: ops layer)
//   [x] Happy: parses +list-themes output and marks the active theme from
//       +show-config's theme key
//   [x] Unhappy: no ghostty binary resolved — clear error, no Runner call
//
// Client.Keybinds (Classification: ops layer)
//   [x] Happy: parses +list-keybinds output
//   [x] Unhappy: no ghostty binary resolved — clear error, no Runner call
//
// Client.Config (Classification: ops layer)
//   [x] Happy: issues `+show-config --default=false --docs=false` and parses it

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func notFoundLookPath(string) (string, error) {
	return "", errors.New("not found")
}

func notFoundStat(string) (os.FileInfo, error) {
	return nil, errors.New("not found")
}

func TestNew_PathHit_UsesResolvedBinary(t *testing.T) {
	c := New(&exec.FakeRunner{}, WithLookPath(func(name string) (string, error) {
		if name == "ghostty" {
			return "/usr/local/bin/ghostty", nil
		}
		return "", errors.New("unexpected lookup")
	}))
	if c.bin != "/usr/local/bin/ghostty" {
		t.Errorf("bin = %q, want /usr/local/bin/ghostty", c.bin)
	}
}

func TestNew_PathMiss_FallsBackToCmuxPath(t *testing.T) {
	c := New(&exec.FakeRunner{},
		WithLookPath(notFoundLookPath),
		WithStat(func(path string) (os.FileInfo, error) {
			if path != fallbackBin {
				t.Fatalf("stat called with %q, want %q", path, fallbackBin)
			}
			return nil, nil
		}),
	)
	if c.bin != fallbackBin {
		t.Errorf("bin = %q, want fallback %q", c.bin, fallbackBin)
	}
}

func TestNew_NeitherResolves_BinStaysEmpty(t *testing.T) {
	c := New(&exec.FakeRunner{}, WithLookPath(notFoundLookPath), WithStat(notFoundStat))
	if c.bin != "" {
		t.Errorf("bin = %q, want empty", c.bin)
	}
}

func TestNew_WithBin_SkipsResolution(t *testing.T) {
	c := New(&exec.FakeRunner{}, WithBin("/custom/ghostty"), WithLookPath(func(string) (string, error) {
		t.Fatal("lookPath must not be called when WithBin is set")
		return "", nil
	}))
	if c.bin != "/custom/ghostty" {
		t.Errorf("bin = %q, want /custom/ghostty", c.bin)
	}
}

// fakeGhosttyRunner answers +list-themes, +list-keybinds, and +show-config
// with canned output keyed on the subcommand (args[0]).
func fakeGhosttyRunner(themesOut, keybindsOut, configOut string) *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			if len(args) == 0 {
				return "", nil
			}
			switch args[0] {
			case "+list-themes":
				return themesOut, nil
			case "+list-keybinds":
				return keybindsOut, nil
			case "+show-config":
				return configOut, nil
			}
			return "", nil
		},
	}
}

func TestClient_Themes_MarksActiveTheme(t *testing.T) {
	fake := fakeGhosttyRunner(
		"artificer-dark (user)\nartificer-light (user)\nAdwaita Dark (resources)\n",
		"",
		"theme = artificer-dark\n",
	)
	c := New(fake, WithBin("ghostty"))

	themes, err := c.Themes(context.Background())
	if err != nil {
		t.Fatalf("Themes: %v", err)
	}
	if len(themes) != 3 {
		t.Fatalf("got %d themes, want 3", len(themes))
	}
	for _, th := range themes {
		want := th.Name == "artificer-dark"
		if th.Active != want {
			t.Errorf("theme %q: Active = %v, want %v", th.Name, th.Active, want)
		}
	}

	call := fake.Calls[0]
	if call.Name != "ghostty" || call.Args[0] != "+list-themes" || call.Args[1] != "--plain" {
		t.Errorf("first call = %+v, want ghostty +list-themes --plain", call)
	}
}

func TestClient_Themes_NoBinary_ClearErrorNoRunnerCall(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithLookPath(notFoundLookPath), WithStat(notFoundStat))

	if _, err := c.Themes(context.Background()); err == nil {
		t.Fatal("expected an error when no ghostty binary resolved")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no Runner call expected, got: %+v", fake.Calls)
	}
}

func TestClient_Keybinds_ParsesOutput(t *testing.T) {
	fake := fakeGhosttyRunner("", "keybind = escape=end_search\n", "")
	c := New(fake, WithBin("ghostty"))

	binds, err := c.Keybinds(context.Background())
	if err != nil {
		t.Fatalf("Keybinds: %v", err)
	}
	if len(binds) != 1 || binds[0].Trigger != "escape" {
		t.Errorf("binds = %+v, want one escape=end_search binding", binds)
	}
}

func TestClient_Keybinds_NoBinary_ClearErrorNoRunnerCall(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithLookPath(notFoundLookPath), WithStat(notFoundStat))

	if _, err := c.Keybinds(context.Background()); err == nil {
		t.Fatal("expected an error when no ghostty binary resolved")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no Runner call expected, got: %+v", fake.Calls)
	}
}

func TestClient_Config_IssuesShowConfigWithFlags(t *testing.T) {
	fake := fakeGhosttyRunner("", "", "theme = artificer-dark\nfont-size = 14\n")
	c := New(fake, WithBin("ghostty"))

	cfg, err := c.Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Theme() != "artificer-dark" {
		t.Errorf("Theme() = %q, want artificer-dark", cfg.Theme())
	}

	call := fake.Last()
	want := []string{"+show-config", "--default=false", "--docs=false"}
	if len(call.Args) != len(want) {
		t.Fatalf("args = %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d = %q, want %q", i, call.Args[i], w)
		}
	}
}
