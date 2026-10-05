package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// sgrFor builds the truecolor SGR parameter string a renderer emits for hex,
// e.g. "38;2;32;32;62" for a foreground.
func sgrFor(t *testing.T, plane int, hex string) string {
	t.Helper()
	h := strings.TrimPrefix(hex, "#")
	if len(h) != 6 {
		t.Fatalf("unexpected hex %q", hex)
	}
	var rgb [3]string
	for i := range rgb {
		v, err := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
		if err != nil {
			t.Fatalf("bad hex %q: %v", hex, err)
		}
		rgb[i] = strconv.Itoa(int(v))
	}
	return fmt.Sprintf("%d;2;%s;%s;%s", plane, rgb[0], rgb[1], rgb[2])
}

func stubFangTrust(t *testing.T, v bool) {
	t.Helper()
	prev := fangTrustsProbe
	fangTrustsProbe = func() bool { return v }
	t.Cleanup(func() { fangTrustsProbe = prev })
}

// TestFangWiring_HonoursProbeSeam runs help and an error frame through the real
// fangOptions with an auto-mode theme. Under `go test` stdout is not a TTY, so
// fang hands the scheme func lipgloss.LightDark(false): a trusted probe must
// therefore render the LIGHT palette and an untrusted one the theme's own
// (dark) answer. A constant passed at the call site turns one arm red.
func TestFangWiring_HonoursProbeSeam(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("NO_COLOR", "")

	th := theme.New(theme.Options{Mode: theme.ModeAuto}, true)
	light, dark := th.WithDark(false), th.WithDark(true)

	cases := []struct {
		name  string
		trust bool
		want  theme.Theme
		other theme.Theme
	}{
		{"trusted probe renders light", true, light, dark},
		{"untrusted probe renders theme dark", false, dark, light},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubFangTrust(t, c.trust)

			// Help: the accent colours the section titles.
			wantHelp := sgrFor(t, 38, c.want.Hex(theme.RoleAccent))
			otherHelp := sgrFor(t, 38, c.other.Hex(theme.RoleAccent))
			if wantHelp == otherHelp {
				t.Fatal("accent identical in light and dark; test would be vacuous")
			}
			root := &cobra.Command{Use: "forgectl", Short: "x", RunE: func(*cobra.Command, []string) error { return nil }}
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"--help"})
			if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef", th)...); err != nil {
				t.Fatalf("help: %v", err)
			}
			if !strings.Contains(out.String(), wantHelp) {
				t.Errorf("help lacks %q; got %q", wantHelp, out.String())
			}
			if strings.Contains(out.String(), otherHelp) {
				t.Errorf("help carries the wrong-background accent %q", otherHelp)
			}

			// Error frame: the header fill.
			wantErr := sgrFor(t, 48, c.want.Hex(theme.RoleUrgentFill))
			otherErr := sgrFor(t, 48, c.other.Hex(theme.RoleUrgentFill))
			root = &cobra.Command{Use: "forgectl", SilenceUsage: true, RunE: func(*cobra.Command, []string) error {
				return errors.New("boom")
			}}
			out.Reset()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(nil)
			if err := fang.Execute(context.Background(), root, fangOptions("0.0.0", "deadbeef", th)...); err == nil {
				t.Fatal("expected an error")
			}
			if wantErr == otherErr {
				t.Fatal("urgent fill identical in light and dark; test would be vacuous")
			}
			if !strings.Contains(out.String(), wantErr) {
				t.Errorf("error frame lacks %q; got %q", wantErr, out.String())
			}
			if strings.Contains(out.String(), otherErr) {
				t.Errorf("error frame carries the wrong-background fill %q", otherErr)
			}
		})
	}
}

func TestFangTrustsProbeFor(t *testing.T) {
	ok := theme.Env{StdinTTY: true, StdoutTTY: true, Term: "xterm-256color"}
	cases := []struct {
		name   string
		probed bool
		env    theme.Env
		want   bool
	}{
		{"tty, clean env", true, ok, true},
		{"fang did not probe", false, ok, false},
		{"stdin not a tty", true, theme.Env{StdoutTTY: true, Term: "xterm"}, false},
		{"NO_COLOR set", true, theme.Env{StdinTTY: true, StdoutTTY: true, Term: "xterm", NoColor: true}, false},
		{"tmux", true, theme.Env{StdinTTY: true, StdoutTTY: true, Term: "tmux-256color"}, false},
		{"screen", true, theme.Env{StdinTTY: true, StdoutTTY: true, Term: "screen"}, false},
		{"stdout not a tty in env", true, theme.Env{StdinTTY: true, Term: "xterm"}, false},
	}
	for _, c := range cases {
		if got := fangTrustsProbeFor(c.probed, c.env); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
