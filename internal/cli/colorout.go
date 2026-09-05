package cli

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"
)

// colorOut wraps a command's stdout in a colorprofile writer, and it is not
// optional decoration — without it the plain-output commands emit truecolor
// escapes into pipes and into NO_COLOR terminals.
//
// Lip Gloss v1 resolved the colour profile inside Style.Render from a package
// global, so a piped or NO_COLOR run produced plain text for free. v2 moved
// that decision to the writer (lipgloss/v2 writer.go): Render always emits the
// full truecolor sequence and colorprofile.Writer downgrades — to 256 colours,
// to 16, or to nothing at all — based on the terminal and the environment.
//
// Bubble Tea and fang each build such a writer for their own output, so this is
// only needed by commands that print styled text directly. Which commands those
// are is enforced by TestStyledPrintsGoThroughColorOut rather than remembered.
//
// "fang builds its own" is not the same as "fang gets it right": fang reads a
// raw os.Environ() (fang@v1.0.0 fang.go:131,174) and so inherits the NO_COLOR
// precedence bug described on colorEnv below. That is fixed once, for every
// writer in the process, by normalizeColorEnv at startup — not here, because a
// writer inside a dependency has no call site to wrap.
//
// PR 2b replaces this with theme.Writer, which does the same thing from the
// theme's side of the boundary.
func colorOut(cmd *cobra.Command) io.Writer {
	return colorprofile.NewWriter(cmd.OutOrStdout(), colorEnv(os.Environ()))
}

// colorEnv returns environ with CLICOLOR_FORCE removed when NO_COLOR is set to
// a non-empty value.
//
// colorprofile applies its NO_COLOR branch only when the destination is a TTY
// (colorprofile@v0.4.3 env.go:86) — reasonably, since a non-TTY is already
// NoTTY and strips everything. But CLICOLOR_FORCE is evaluated afterwards and
// promotes that NoTTY profile to ANSI, so the two together emit colour into a
// pipe: measured at 12 escape sequences from `forgectl doctor` before this.
//
// https://no-color.org specifies NO_COLOR as absolute — "when present and not
// an empty string (regardless of its value)" — so it outranks a force flag, and
// dropping CLICOLOR_FORCE here is what makes that true. The empty-string case
// is deliberately NOT honoured, per the same sentence.
func colorEnv(environ []string) []string {
	if !noColorSet(environ) {
		return environ
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, "CLICOLOR_FORCE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// normalizeColorEnv makes NO_COLOR authoritative for every colour writer this
// process will build, including the ones inside dependencies.
//
// colorEnv fixes the environment handed to colorOut, but fang constructs its
// own writers from a raw os.Environ() for help and error output, and Bubble Tea
// does the same. Measured before this existed: `forgectl --help` emitted 42
// escape sequences into a pipe under NO_COLOR=1 with CLICOLOR_FORCE=1 set, and
// a fang error frame emitted 2. There is no call site to wrap for those, so the
// process environment is normalised once instead.
//
// Two edits, both narrowing:
//
//   - CLICOLOR_FORCE is removed, so it cannot promote a NoTTY profile to ANSI.
//   - NO_COLOR is rewritten to "1". colorprofile parses it with strconv.ParseBool
//     (colorprofile@v0.4.3 env.go:116), so a spec-legal NO_COLOR=yes fails to
//     parse and the branch never fires — which no-color.org's "regardless of its
//     value" forbids. On a pipe this was masked by the profile already being
//     NoTTY; on a TTY it meant colour despite NO_COLOR.
//
// A no-op unless NO_COLOR is set to a non-empty value, and it only ever removes
// colour, so it cannot surprise a caller into more output than they asked for.
func normalizeColorEnv() {
	if !noColorSet(os.Environ()) {
		return
	}
	_ = os.Unsetenv("CLICOLOR_FORCE")
	_ = os.Setenv("NO_COLOR", "1")
}

// noColorSet reports whether NO_COLOR is present with a non-empty value. A
// later assignment wins, matching how a process's environment resolves.
func noColorSet(environ []string) bool {
	set := false
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "NO_COLOR="); ok {
			set = v != ""
		}
	}
	return set
}
