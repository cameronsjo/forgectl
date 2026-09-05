package theme

import (
	"io"
	"strings"

	"github.com/charmbracelet/colorprofile"
)

// Writer wraps w in a colorprofile writer built from env, so styled text
// downgrades (or drops entirely) to match the destination's colour profile
// instead of always emitting truecolor escapes.
//
// This is internal/cli/colorout.go's colorEnv precedence fix, moved to the
// theme side of the leaf boundary (internal/cli/colorout.go documents the
// same bug and says as much: "PR 2b replaces this with theme.Writer"):
// colorprofile only special-cases NO_COLOR when the destination is a TTY, so
// on a pipe CLICOLOR_FORCE survives unfiltered and promotes a NoTTY profile
// to ANSI — 12 escape sequences leaked from a piped `forgectl doctor` before
// this fix existed. https://no-color.org makes NO_COLOR absolute ("when
// present and not an empty string, regardless of its value"), so it must
// outrank a force flag, and dropping CLICOLOR_FORCE here is what makes that
// true.
func (t Theme) Writer(w io.Writer, env []string) io.Writer {
	return colorprofile.NewWriter(w, colorEnv(env))
}

// colorEnv returns env with CLICOLOR_FORCE removed when NO_COLOR is set to a
// non-empty value; otherwise env is returned untouched.
func colorEnv(env []string) []string {
	if !noColorSet(env) {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLICOLOR_FORCE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// noColorSet reports whether NO_COLOR is present with a non-empty value in
// env. A later assignment wins, matching how a process environment resolves
// duplicate keys.
func noColorSet(env []string) bool {
	set := false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "NO_COLOR="); ok {
			set = v != ""
		}
	}
	return set
}
