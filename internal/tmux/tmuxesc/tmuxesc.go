// Package tmuxesc holds the pure string escapes that make an operand survive
// tmux's command parser and format expansion. It is a leaf, importing only the
// standard library, so that internal/tmux and internal/exec can share one
// spelling: exec applies DirOperand to a sealed argument without handing the
// payload to any caller (forgectl#839).
package tmuxesc

import "strings"

// ArgvSeparator makes an argv operand survive tmux's command splitter
// (forgectl#823). cmd_parse_from_arguments (cmd-parse.y, byte-identical in
// tmux 3.4 and 3.7c) treats any argv element ending in ';' as the end of a
// command: it strips the ';' and starts a new command with the next element.
// The same function supports one escape: when the character left before the
// stripped ';' is a backslash, that backslash becomes the ';' and the command
// does not end. So replacing a trailing ';' with `\;` always lands the operand
// as given, whatever precedes it: "x;" is sent as `x\;`, `x\;` as `x\\;`,
// and a lone ";" as `\;`.
//
// Unescaped, a directory ending in ';' silently gave a session or window a
// different working directory (measured on tmux 3.4: `-c "<dir>;"` landed in
// <dir>), and a command argument ending in ';' was cut short, with every later
// argument read as a tmux command of its own. Escaping rather than refusing
// keeps ordinary commands working, such as `sh -c 'a; b;'` or
// `find . -exec rm {} \;`. An operand without a trailing ';' is returned
// unchanged, so every everyday argv is byte-identical.
func ArgvSeparator(s string) string {
	if !strings.HasSuffix(s, ";") {
		return s
	}
	return s[:len(s)-1] + `\;`
}

// Format escapes s so that tmux's format expansion hands back exactly s
// (forgectl#806). tmux 3.4 format-expands a new-session -s name, a new-window
// -n name, and a rename-session name, bare or guarded alike, so an
// operator-typed `#(cmd)` starts a shell job and `#{pid}` lands as a number.
// That is not a privilege boundary for a typed name, but `forgectl open`
// names its session after a directory, and a name that silently lands as
// something else breaks every later exact-name resolve.
//
// The rule is NOT a plain '#' -> '##': tmux keeps a run of '#' that is
// directly followed by '[' verbatim (a style escape), so "#[x" lands as "#[x"
// and "##[x" as "##[x" — doubling those would add bytes. Every other '#' is
// doubled, and "##" expands back to one. Measured on tmux 3.4 against an
// isolated socket: 2,100 random names over "#[]{}(),?=aHS '", each through a
// guarded rename and an argv create, landed byte for byte.
func Format(s string) string {
	if !strings.Contains(s, "#") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) * 2)
	for i := 0; i < len(s); {
		if s[i] != '#' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '#' {
			j++
		}
		b.WriteString(s[i:j])
		if j == len(s) || s[j] != '[' {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

// DirOperand spells a directory for a tmux `-c` operand so that it
// lands exactly as given (forgectl#839). Two tmux layers read that operand,
// and it needs an escape for each:
//
//   - the argv splitter, which ends a command at an element ending in ';'
//     (ArgvSeparator), and
//   - format expansion. new-session expands its -c with format_single
//     (cmd-new-session.c), and spawn expands the pane's -c again (spawn.c),
//     in tmux 3.4 and 3.7c alike. Unescaped, a directory whose path holds
//     `#(cmd)` runs cmd in the tmux server, and one holding `#{...}` or `##`
//     is rewritten, so the session starts in $HOME instead. A cloned repo or
//     an extracted archive can name a directory that way.
//
// The two escapes touch disjoint bytes (a trailing ';' and '#'), so their
// order does not matter. A path with neither is returned unchanged.
//
// internal/exec applies it to a sealed -c operand through its closed
// Transform set (exec.TmuxDirOperand), for internal/surface/tmuxadapter.
func DirOperand(dir string) string {
	return Format(ArgvSeparator(dir))
}
