// Package forgive normalizes user input and owns the canonical tmux verb
// registry, so the CLI shrugs off iOS autocorrect noise and fat-fingered
// aliases. Pure stdlib by design — the cobra wiring that consumes this lives
// in internal/cli.
package forgive

import "strings"

// Normalize cleans a single candidate verb so iOS autocorrect and stray
// punctuation don't cause a miss. It lowercases, trims surrounding
// whitespace, and strips trailing punctuation/space. Examples:
//
//	"LS. "  -> "ls"
//	"Kill," -> "kill"
//	"  Tree " -> "tree"
func Normalize(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	return strings.TrimRight(lower, " \t.,!?;:")
}

// ProjectAliases maps each canonical projects verb to its accepted aliases.
// Same single-source-of-truth pattern as TmuxAliases.
var ProjectAliases = map[string][]string{
	"pick":  {"p", "open"},
	"list":  {"l", "ls", "find"},
	"clone": {"c"},
}

// LaunchAliases maps each canonical launch subcommand to its accepted aliases.
// The `cl` shorthand for the launch group itself is a Cobra alias on the parent
// command (see newLaunchCmd), not a subcommand alias, so it is not listed here.
var LaunchAliases = map[string][]string{
	"which": {"config"},
}

// WorkflowAliases maps each canonical workflow subcommand to its accepted
// aliases. The `flow` shorthand for the workflow group itself is a Cobra
// alias on the parent command (see newWorkflowCmd), not a subcommand alias,
// so it is not listed here.
var WorkflowAliases = map[string][]string{
	"run": {"r"},
}

// BenchAliases maps each canonical bench subcommand to its accepted aliases.
var BenchAliases = map[string][]string{
	"status": {"health", "st"},
}

// DockerAliases maps each canonical docker subcommand to its accepted
// aliases. Same single-source-of-truth pattern as BenchAliases.
var DockerAliases = map[string][]string{
	"build": {"b"},
	"run":   {"r"},
	"shell": {"sh", "attach"},
}

// TmuxAliases maps each canonical tmux verb to its accepted aliases. This is
// the single source of truth: internal/cli builds cobra command Aliases by
// iterating this map, and Canonical uses it for known-verb detection in the
// unknown-verb -> TUI fallthrough.
var TmuxAliases = map[string][]string{
	"ls":      {"l", "list", "sessions"},
	"pick":    {"p", "go", "n", "new"},
	"kill":    {"k", "rm", "delete", "x"},
	"rename":  {"mv", "rn"},
	"windows": {"w"},
	"tree":    {"t"},
	"last":    {"-"},
	"cheat":   {"keys"},
}

// aliasToCanonical is a reverse lookup: alias -> canonical verb.
// Built once at package init from TmuxAliases.
var aliasToCanonical map[string]string

func init() {
	aliasToCanonical = make(map[string]string)
	for canonical, aliases := range TmuxAliases {
		for _, alias := range aliases {
			aliasToCanonical[alias] = canonical
		}
	}
}

// Canonical resolves a raw token (already user-supplied, not yet normalized)
// to its canonical tmux verb. It normalizes the token first. If the token is
// itself a canonical verb, it returns (verb, true). If it is a registered
// alias, it returns the canonical verb it maps to, true. Otherwise it returns
// ("", false) — the signal the caller uses to fall through to the TUI.
func Canonical(token string) (canonical string, known bool) {
	normalized := Normalize(token)
	if _, isCanonical := TmuxAliases[normalized]; isCanonical {
		return normalized, true
	}
	if canon, ok := aliasToCanonical[normalized]; ok {
		return canon, true
	}
	return "", false
}
