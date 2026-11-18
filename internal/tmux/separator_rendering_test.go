//go:build unix

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// requireEscapedSeparatorEnv is set by the CI leg that runs on a tmux which
// renders the field separator escaped (ubuntu-24.04, tmux 3.4). There the
// unreadable-row tests (forgectl#826, #836, #815) are the only real-tmux check
// of that path, so a skip must fail the leg instead of looking like a pass,
// the way FORGECTL_REQUIRE_TMUX does for a missing tmux binary. On a tmux that
// renders the separator raw, or refuses the name, a skip is correct and the
// variable is unset.
const requireEscapedSeparatorEnv = "FORGECTL_REQUIRE_ESCAPED_SEPARATOR"

// skipOrRequireEscaped is t.Skipf, except that it fails the test when
// requireEscapedSeparatorEnv is set: the leg that sets it exists to run the
// original assertions, so a skip there means the leg has gone quiet.
func skipOrRequireEscaped(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireEscapedSeparatorEnv) != "" {
		t.Fatalf("%s=1 marks the CI leg whose tmux must render the separator escaped (ubuntu-24.04, "+
			"tmux 3.4), but this test would skip. The image's tmux may have moved to one that renders it "+
			"raw or refuses the name, and the leg would pass without running the check: "+format,
			append([]any{requireEscapedSeparatorEnv}, args...)...)
	}
	t.Skipf(format, args...)
}

// detectSeparatorRendering reports whether this tmux prints a 0x1F in -F output
// as the four characters `\037` (escaped) instead of the raw byte. It fails
// closed: any other rendering, such as the `_` substitution tmux 3.6a does
// without -u under LANG=C, or an empty listing, is an error rather than a guess.
//
// It runs `-u list-sessions`, as Client.tmuxArgs does, so it measures the
// rendering the parsers see. Measured on 3.4 and 3.6a: list-sessions,
// list-windows, list-panes and display-message render alike within a version
// (format.go, escapedFieldSep). It needs a running server.
func detectSeparatorRendering(ctx context.Context, runner internalexec.Runner, tmuxBin string) (escaped bool, err error) {
	out, err := runner.Run(ctx, tmuxBin, "-u", "list-sessions", "-F", "x"+FieldSep+"y")
	if err != nil {
		return false, fmt.Errorf("list-sessions probe: %w", err)
	}
	line, _, _ := strings.Cut(out, "\n")
	switch line {
	case "x" + FieldSep + "y":
		return false, nil
	case "x" + escapedFieldSep + "y":
		return true, nil
	default:
		return false, fmt.Errorf("list-sessions rendered 0x1F as %q, which is neither raw nor %q", line, escapedFieldSep)
	}
}

// rendersSeparatorEscaped is detectSeparatorRendering for a test: it fails the
// test on an unknown rendering and logs the one it detected, so a CI log says
// which branch a test took.
//
// This, not name storage, is what separates tmux 3.4 from 3.6a here. Both
// ACCEPT a name carrying 0x1F and both store it as the text `\037` (measured:
// #{n:window_name} is 10 for "hid<0x1F>den" on 3.4 and on 3.6a), so no stored
// name holds the byte. 3.4 then renders every real separator as `\037` too,
// which makes the stored text indistinguishable from a separator: the row
// gains fields and is dropped as unreadable. 3.6a renders the separator raw, so
// the same name is plain text and the row stays readable. 3.7 and later refuse
// the name outright. A caller branches on this to pick the outcome that
// rendering must produce.
func rendersSeparatorEscaped(t *testing.T, runner internalexec.Runner, tmuxBin string) bool {
	t.Helper()
	escaped, err := detectSeparatorRendering(context.Background(), runner, tmuxBin)
	if err != nil {
		t.Fatalf("%v", err)
	}
	version, _ := runner.Run(context.Background(), tmuxBin, "-V")
	rendering := "raw"
	if escaped {
		rendering = escapedFieldSep
	}
	t.Logf("%s renders the field separator as %s under -u", strings.TrimSpace(version), rendering)
	return escaped
}

// skipUnreadableRowAbsent ends a test whose subject, a row tmux could not
// render readably, cannot exist on a tmux that renders the separator raw. The
// readable outcome was asserted first. The result is a SKIP, not a PASS of the
// original check: pane_rows_integration_test.go skips for the same gap. The
// parse itself stays covered by the unit tests over the row parsers
// (TestParseWindowRowsCountsDroppedRows, all_unreadable_test.go), which feed
// them rows whose name carries the separator, in both renderings; the real-tmux
// check runs on the CI leg that sets requireEscapedSeparatorEnv.
func skipUnreadableRowAbsent(t *testing.T, runner internalexec.Runner, tmuxBin, what string) {
	t.Helper()
	version, _ := runner.Run(context.Background(), tmuxBin, "-V")
	skipOrRequireEscaped(t, "%s renders the field separator raw, so %s is plain text and its row is readable "+
		"(asserted above); there is no unreadable row to count on this tmux", strings.TrimSpace(version), what)
}

// probeRunner answers every call with one canned reply and records the argv.
type probeRunner struct {
	internalexec.FakeRunner
	out  string
	err  error
	args [][]string
}

func (p *probeRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	p.args = append(p.args, slices.Clone(args))
	return p.out, p.err
}

// TestDetectSeparatorRendering pins the probe the field-separator tests branch
// on. A probe that guessed on an unknown output would send a test down the
// wrong branch and still pass, so every output but the two known renderings
// must be an error.
//
// Mutation that turns it red: make the default case return (false, nil).
func TestDetectSeparatorRendering(t *testing.T) {
	boom := errors.New("no server running")
	for name, tt := range map[string]struct {
		out     string
		runErr  error
		escaped bool
		wantErr bool
	}{
		"raw":                       {out: "x" + FieldSep + "y\n", escaped: false},
		"raw, one line per session": {out: "x" + FieldSep + "y\nx" + FieldSep + "y\n", escaped: false},
		"escaped":                   {out: `x\037y` + "\n", escaped: true},
		"escaped, no newline":       {out: `x\037y`, escaped: true},
		"underscore substitution":   {out: "x_y\n", wantErr: true},
		"separator dropped":         {out: "xy\n", wantErr: true},
		"empty output":              {out: "", wantErr: true},
		"a session name":            {out: "base\n", wantErr: true},
		"run failure":               {runErr: boom, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &probeRunner{out: tt.out, err: tt.runErr}
			escaped, err := detectSeparatorRendering(context.Background(), runner, "tmux")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && escaped != tt.escaped {
				t.Fatalf("escaped = %v, want %v", escaped, tt.escaped)
			}
			if tt.runErr != nil && !errors.Is(err, boom) {
				t.Fatalf("err = %v, want it to wrap the run failure", err)
			}
			want := []string{"-u", "list-sessions", "-F", "x" + FieldSep + "y"}
			if len(runner.args) != 1 || !slices.Equal(runner.args[0], want) {
				t.Fatalf("probe argv = %q, want exactly %q", runner.args, want)
			}
		})
	}
}
