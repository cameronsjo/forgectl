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
// Bubble Tea and fang each own such a writer for their own output, so this is
// only needed by commands that print styled text directly. Which commands those
// are is enforced by TestStyledPrintsGoThroughColorOut rather than remembered.
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
