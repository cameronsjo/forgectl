package cli

import (
	"io"
	"os"

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
// only needed by commands that print styled text directly: doctor, launch
// doctor, launch which, pr dash, pr prs.
//
// PR 2b replaces this with theme.Writer, which does the same thing from the
// theme's side of the boundary.
func colorOut(cmd *cobra.Command) io.Writer {
	return colorprofile.NewWriter(cmd.OutOrStdout(), os.Environ())
}
