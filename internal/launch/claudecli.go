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
// or when any argument before Claude's own `--` selects print mode or asks
// only for help or the version.
//
// Posture flags do not belong on any of these. Print mode is what scripts
// run, and a script expects plain `claude -p` rather than the profile's plan
// mode and model. A subcommand can be broken outright by the injected flags:
// the variadic `--add-dir <directories...>` swallows `mcp list` as two more
// directories, and claude then starts a session instead (Claude Code
// 2.1.285).
//
// Only args[0] is checked for a subcommand. Finding the first positional past
// the flags would mean knowing which Claude flags take a value, and a
// hand-kept table of that would drift silently.
func IsClaudePassthrough(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] != "agents" && isClaudeSubcommand(args[0]) {
		return true
	}
	for _, a := range args {
		switch {
		case a == "--":
			// Everything after Claude's own separator is prompt text.
			return false
		case a == "-p", a == "--print",
			a == "--output-format", strings.HasPrefix(a, "--output-format="),
			a == "-h", a == "--help",
			a == "-v", a == "--version":
			return true
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
