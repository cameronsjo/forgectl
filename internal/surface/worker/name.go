// Package worker holds the pieces a coordinator needs to start worker agents
// in their own git worktrees: the name rules, the worktree helper, and the
// ledger that records what each launch created.
package worker

import (
	"errors"
	"strings"
)

// maxNameLen bounds a worker name. It becomes a directory name and a ledger
// key, and an operator reads it in `surface list`; nothing longer is useful.
const maxNameLen = 48

// ErrInvalidName reports a worker name outside the allowed shape.
var ErrInvalidName = errors.New("worker: name must be 1-48 characters of a-z, 0-9 and '-', starting with a letter or digit")

// ValidName reports whether name is usable as a worker name.
//
// The rule is narrow on purpose. The name becomes a single path component
// under <repo>/.claude/worktrees/, so a separator, a dot segment, or a leading
// dash would let it point somewhere else or read as a flag. Lowercase only, so
// two names that differ only in case cannot collide on a case-insensitive
// filesystem.
func ValidName(name string) error {
	if name == "" || len(name) > maxNameLen {
		return ErrInvalidName
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return ErrInvalidName
		}
	}
	return nil
}

// ErrInvalidBranch reports a branch name refused before it reaches git.
var ErrInvalidBranch = errors.New("worker: branch name is not usable")

// precheckBranch refuses the shapes that are dangerous before git sees them: a
// leading dash reads as an option, and control characters reach a terminal
// when git echoes the name back. git's own check-ref-format runs after this.
func precheckBranch(branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return ErrInvalidBranch
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return ErrInvalidBranch
		}
	}
	return nil
}
