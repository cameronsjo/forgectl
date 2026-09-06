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

// colorEnv normalises env so NO_COLOR is authoritative, or returns it
// untouched when NO_COLOR is not set.
//
// Two edits, both narrowing. CLICOLOR_FORCE is removed so it cannot promote a
// NoTTY profile to ANSI. And NO_COLOR is rewritten to "1", because
// colorprofile parses the value with strconv.ParseBool
// (colorprofile@v0.4.3 env.go:116) — so a spec-legal NO_COLOR=purple fails to
// parse and the branch never fires, which no-color.org's "regardless of its
// value" forbids. On a pipe that was masked by the profile already being
// NoTTY; with a forced TTY profile it meant colour despite NO_COLOR.
func colorEnv(env []string) []string {
	if !noColorSet(env) {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLICOLOR_FORCE=") || strings.HasPrefix(kv, "NO_COLOR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "NO_COLOR=1")
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
