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

// IsClaudeSubcommandCall reports whether args runs a non-session Claude
// subcommand (`mcp`, `doctor`, `update`, …), which must reach claude
// byte-clean with no injected posture and no banner. It starts no session, so
// there is no posture for the profile to set, and the injected flags can break
// it outright: the variadic `--add-dir <directories...>` swallows `mcp list` as
// two more directories, and claude then starts a session instead (Claude Code
// 2.1.285).
//
// A subcommand after a leading `--` counts too, because claude dispatches it
// anyway: `claude -- mcp list` runs `mcp list` (2.1.285). That is the argv
// `forgectl launch -- -- mcp list` leaves once forgectl consumes its own `--`.
//
// Only that first positional slot is checked, because it is the one slot that
// can never be a flag's value. Finding the first positional past arbitrary
// flags would mean knowing which Claude flags take a value, and a hand-kept
// table of that would drift silently.
//
// "agents" is never counted here, in either slot: it starts sessions. A
// leading `agents` gets its own posture branch in selectPosture. `-- agents …`
// falls through to BuilderArgs, which still injects the profile posture ahead
// of claude's `--`. It is not rerouted to AgentsArgs because claude reads
// everything after its `--` as operands: `claude -- agents --json` fails "too
// many arguments for 'agents'" (2.1.285), so it is not the same command as
// `claude agents --json`.
func IsClaudeSubcommandCall(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := args[0]
	if sub == "--" && len(args) > 1 {
		sub = args[1]
	}
	return sub != "agents" && isClaudeSubcommand(sub)
}

// IsClaudeHelpOrVersion reports whether args[0] is `-h`, `--help`, `-v`, or
// `--version`. That run prints and exits without starting a session, so it
// reaches claude byte-clean.
//
// Only args[0] counts, because anywhere later the token can be a flag's value.
// `claude -p --append-system-prompt --help "<task>"` consumes `--help` as the
// system prompt and RUNS the task (2.1.285). If a scan anywhere in argv
// matched it, that run would reach claude with no permission mode at all. A
// later help flag falls through to the print or builder posture instead, and
// claude still just prints help: `claude --permission-mode plan -p x --help`
// prints help.
func IsClaudeHelpOrVersion(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "-v", "--version":
		return true
	}
	return false
}

// claudeNoValueFlags are the top-level `claude` options that never consume a
// following dash-prefixed token (`claude --help`, Claude Code 2.1.285). That
// covers the boolean flags, and also the optional-value ones (`--resume
// [value]`, `--debug [filter]`, `-w [name]`, …), because an optional value is
// only taken when the next token does not start with "-": `claude --debug
// --version` and `claude -r --version` print the version, while `claude
// --append-system-prompt --version` consumes it as the prompt (2.1.285).
// IsClaudePrintMode uses the list to tell a flag in flag position from one
// that sits in a value slot.
//
// If the list drifts, the outcome is safe. An unknown flag is assumed to take
// a value, so a print flag after it reads as a value and the run gets the
// builder posture, which still carries the permission mode.
// TestClaudeFlagLists_MatchInstalledHelp pins the list to the installed
// `claude --help`, so a listed flag that starts taking a value fails a test
// rather than silently routing interactive runs to print mode.
var claudeNoValueFlags = map[string]bool{
	"--allow-dangerously-skip-permissions": true,
	"--ax-screen-reader":                   true,
	"--bare":                               true,
	"--bg":                                 true,
	"--background":                         true,
	"--brief":                              true,
	"--chrome":                             true,
	"--cloud":                              true,
	"-c":                                   true,
	"--continue":                           true,
	"--dangerously-skip-permissions":       true,
	"-d":                                   true,
	"--debug":                              true,
	"--desktop":                            true,
	"--disable-slash-commands":             true,
	"--exclude-dynamic-system-prompt-sections": true,
	"--fork-session":             true,
	"--forward-subagent-text":    true,
	"--from-pr":                  true,
	"-h":                         true,
	"--help":                     true,
	"--ide":                      true,
	"--include-hook-events":      true,
	"--include-partial-messages": true,
	"--no-chrome":                true,
	"--no-session-persistence":   true,
	"-p":                         true,
	"--print":                    true,
	"--prompt-suggestions":       true,
	"--remote-control":           true,
	"--replay-user-messages":     true,
	"--restricted":               true,
	"-r":                         true,
	"--resume":                   true,
	"--safe-mode":                true,
	"--strict-mcp-config":        true,
	"--teleport":                 true,
	"--tmux":                     true,
	"--verbose":                  true,
	"-v":                         true,
	"--version":                  true,
	"-w":                         true,
	"--worktree":                 true,
}

// claudeValueFlags are the top-level `claude` options that take a required
// value (`<value>` in `claude --help`, Claude Code 2.1.285). A required value
// is taken whatever it looks like, `--` included: `claude
// --append-system-prompt -- -p hi` runs in print mode with "--" as the
// system prompt (2.1.285). IsClaudePrintMode reads a `--` as a value only
// after one of these.
//
// If the list drifts, the outcome is safe. A `--` after an unknown flag is
// taken as claude's end of options, so no print flag after it counts, and the
// run gets the builder posture, which still carries the permission mode.
// TestClaudeFlagLists_MatchInstalledHelp pins this list and
// claudeNoValueFlags to the installed `claude --help`.
var claudeValueFlags = map[string]bool{
	"--add-dir":                            true,
	"--agent":                              true,
	"--agents":                             true,
	"--allowedTools":                       true,
	"--allowed-tools":                      true,
	"--append-system-prompt":               true,
	"--autocompact":                        true,
	"--betas":                              true,
	"--client-data-url":                    true,
	"--debug-file":                         true,
	"--disallowedTools":                    true,
	"--disallowed-tools":                   true,
	"--effort":                             true,
	"--environment":                        true,
	"--fallback-model":                     true,
	"--file":                               true,
	"--input-format":                       true,
	"--json-schema":                        true,
	"--max-budget-usd":                     true,
	"--mcp-config":                         true,
	"--model":                              true,
	"-n":                                   true,
	"--name":                               true,
	"--output-format":                      true,
	"--permission-mode":                    true,
	"--permission-prompts":                 true,
	"--plugin-dir":                         true,
	"--plugin-url":                         true,
	"--remote-control-session-name-prefix": true,
	"--session-id":                         true,
	"--setting-sources":                    true,
	"--settings":                           true,
	"--system-prompt":                      true,
	"--system-prompt-snapshot":             true,
	"--tools":                              true,
}

// IsClaudePrintMode reports whether args selects print mode: `-p`, `--print`,
// or `--output-format` (which only works with --print), before Claude's own
// `--` and in flag position. Print mode is what scripts run, so it gets the
// print posture (PrintArgs) rather than the full builder posture.
//
// A print flag in a value slot is that option's value, not print mode:
// `--append-system-prompt -p "task"` is an interactive run whose system prompt
// is "-p". Matching it would send that run to PrintArgs and drop the model,
// effort, and add-dir the builder posture gives it.
//
// A `--` is claude's end of options only in flag position. After a known
// required-value flag (claudeValueFlags) it is that flag's value, and the scan
// goes on past it (forgectl#766).
//
// The scan tracks what kind of slot each token sits in, not just the token
// before it. A known value flag that is itself taken as a value takes
// nothing: in `--model --model -- -p x` the second `--model` is the first
// one's value, so the `--` ends the options and the run is interactive.
//
// Glued short flags (`-cp`, `-pc`) are deliberately not print mode. Claude
// Code 2.1.285 does not treat them as print mode either: under a terminal,
// `claude -c -p` takes the print path and fails fast for want of a prompt,
// while `claude -cp` and `claude -pc` open the interactive session. So the
// builder posture is the one that fits them.
func IsClaudePrintMode(args []string) bool {
	slot := slotFlag
	for _, a := range args {
		cur := slot
		switch {
		case a == "--" && cur == slotKnownValue:
			// The option's value, not the end of options.
		case a == "--":
			return false
		case cur == slotFlag && isPrintFlag(a):
			return true
		}
		slot = nextSlot(a, cur)
	}
	return false
}

// argSlot is what IsClaudePrintMode knows about the position a token sits in.
type argSlot int

const (
	// slotFlag: nothing is waiting for a value, so the token is a flag or a
	// positional.
	slotFlag argSlot = iota
	// slotKnownValue: a flag in claudeValueFlags, itself in flag position,
	// is waiting for this token as its value.
	slotKnownValue
	// slotMaybeValue: the token may be some option's value. It follows a
	// dash-prefixed token that is not known to take nothing. That token is
	// an unknown flag, or one that may itself have been a value.
	slotMaybeValue
)

// nextSlot is the slot of the token after a, which sat in cur. A value a
// known flag took leaves the next token in flag position. Otherwise the
// token after a is in flag position when inFlagPosition says so, a known
// value slot when a is a known value flag in flag position, and a maybe
// value slot in every other case. So an unknown flag still hides a print
// flag after it, and the run keeps the builder posture.
func nextSlot(a string, cur argSlot) argSlot {
	switch {
	case cur == slotKnownValue:
		return slotFlag
	case inFlagPosition(a, claudeNoValueFlags):
		return slotFlag
	case cur == slotFlag && claudeValueFlags[a]:
		return slotKnownValue
	default:
		return slotMaybeValue
	}
}

// isPrintFlag reports whether a is `-p`, `--print`, or `--output-format`, or
// the `<flag>=<value>` form of one of them. A glued short cluster such as
// `-cp` is not one (see IsClaudePrintMode).
func isPrintFlag(a string) bool {
	for _, f := range []string{"-p", "--print", "--output-format"} {
		if a == f || strings.HasPrefix(a, f+"=") {
			return true
		}
	}
	return false
}

// inFlagPosition reports whether the token after prev is in flag position,
// meaning prev cannot be waiting for it as a value. That is the case when prev
// is a bare token (a positional, or a value some flag already took), a
// `--flag=value`, or a flag in noValue. Any other dash-prefixed prev is
// assumed to take a value.
//
// A prev of `--` is in flag position too. Both scans stop at a `--` that ends
// the options, so a `--` they scanned past was some option's value.
func inFlagPosition(prev string, noValue map[string]bool) bool {
	return prev == "--" || !strings.HasPrefix(prev, "-") || strings.Contains(prev, "=") || noValue[prev]
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
