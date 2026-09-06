package cli

import (
	"os"
	"strings"
)

// normalizeColorEnv makes NO_COLOR authoritative for every colour writer this
// process will build, including the ones inside dependencies.
//
// theme.Writer fixes the environment handed to writers forgectl constructs,
// but fang constructs its OWN for help and error output from a raw
// os.Environ() (fang@v1.0.0 fang.go:131,174), and Bubble Tea does the same.
// Measured before this existed: `forgectl --help` emitted 42 escape sequences
// into a pipe under NO_COLOR=1 with CLICOLOR_FORCE=1 set, and a fang error
// frame emitted 2. There is no call site to wrap for those, so the process
// environment is normalised once instead.
//
// Two edits, both narrowing:
//
//   - CLICOLOR_FORCE is removed, so it cannot promote a NoTTY profile to ANSI.
//     colorprofile applies its NO_COLOR branch only when the destination is a
//     TTY, then evaluates CLICOLOR_FORCE unconditionally — so on a pipe the
//     first never runs and the second re-enables colour.
//   - NO_COLOR is rewritten to "1". colorprofile parses it with
//     strconv.ParseBool (colorprofile@v0.4.3 env.go:116), so a spec-legal
//     NO_COLOR=yes fails to parse and the branch never fires — which
//     no-color.org's "regardless of its value" forbids.
//
// A no-op unless NO_COLOR is set to a non-empty value, and it only ever
// removes colour, so it cannot surprise a caller into more output than they
// asked for. theme.Writer applies the same rule to the env slice it is handed;
// this is the process-wide half, for the writers forgectl does not build.
func normalizeColorEnv() {
	if !noColorSet(os.Environ()) {
		return
	}
	_ = os.Unsetenv("CLICOLOR_FORCE")
	_ = os.Setenv("NO_COLOR", "1")
}

// noColorSet reports whether NO_COLOR is present with a non-empty value. A
// later assignment wins, matching how a process's environment resolves
// duplicate keys.
func noColorSet(environ []string) bool {
	set := false
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "NO_COLOR="); ok {
			set = v != ""
		}
	}
	return set
}
