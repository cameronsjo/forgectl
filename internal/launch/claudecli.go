package launch

import "strings"

// claudeSubcommands is every command name (aliases included) that Claude Code
// lists under "Commands:" in `claude --help`. It is the one place forgectl
// knows Claude's subcommand surface. TestClaudeSubcommands_MatchInstalledHelp
// fails when the installed `claude` lists a different set, so a new Claude
// subcommand shows up as a red test rather than as a session that silently
// opens in its place.
//
// "agents" is listed because Claude lists it, but selectPosture routes it
// first: it keeps its own posture-injecting and scripting-passthrough
// branches.
var claudeSubcommands = []string{
	"agents",
	"attach",
	"auth",
	"auto-mode",
	"doctor",
	"gateway",
	"import",
	"install",
	"logs",
	"mcp",
	"plugin", "plugins",
	"project",
	"respawn",
	"rm",
	"setup-token",
	"stop", "kill",
	"ultrareview",
	"update", "upgrade",
}

// isClaudeSubcommand reports whether tok is one of claudeSubcommands.
func isClaudeSubcommand(tok string) bool {
	for _, s := range claudeSubcommands {
		if s == tok {
			return true
		}
	}
	return false
}

// IsClaudePassthrough reports whether a Claude argv must reach claude
// byte-clean, with no injected posture and no banner. That is the case when
// args[0] is a non-session Claude subcommand (`mcp`, `doctor`, `update`, …),
// or when any argument before Claude's own `--` asks only for help or the
// version. Neither starts a session, so there is no posture for the profile to
// set, and the injected flags can break a subcommand outright: the variadic
// `--add-dir <directories...>` swallows `mcp list` as two more directories,
// and claude then starts a session instead (Claude Code 2.1.285).
//
// A subcommand after a leading `--` counts too, because claude dispatches it
// anyway: `claude -- mcp list` runs `mcp list` (2.1.285). That is the argv
// `forgectl launch -- -- mcp list` leaves once forgectl consumes its own `--`.
//
// Only that first positional slot is checked. Finding the first positional
// past arbitrary flags would mean knowing which Claude flags take a value, and
// a hand-kept table of that would drift silently.
//
// "agents" is never a passthrough here, in either slot: it starts sessions,
// so it keeps the posture-injecting branch selectPosture gives it.
func IsClaudePassthrough(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := args[0]
	if sub == "--" && len(args) > 1 {
		sub = args[1]
	}
	if sub != "agents" && isClaudeSubcommand(sub) {
		return true
	}
	return scanClaudeFlags(args, "-h", "--help", "-v", "--version")
}

// IsClaudePrintMode reports whether any argument before Claude's own `--`
// selects print mode: `-p`, `--print`, or `--output-format` (which only works
// with --print). Print mode is what scripts run, so it gets the print posture
// (PrintArgs) rather than the full builder posture.
func IsClaudePrintMode(args []string) bool {
	return scanClaudeFlags(args, "-p", "--print", "--output-format")
}

// scanClaudeFlags reports whether any argument before Claude's own `--`
// separator equals one of flags, or is `<flag>=<value>` for one of them.
// Everything after that separator is prompt text.
func scanClaudeFlags(args []string, flags ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

// ConsumeLeadingSeparator drops one leading "--" from args. `forgectl launch
// -- <args>` means "these are harness args, not a forgectl launch verb", so
// the separator is forgectl's and the harness never sees it. Only the first
// one is dropped, so `-- -- text` still hands the harness its own `--`.
func ConsumeLeadingSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}
