// Package validated holds the one command shape internal/exec starts a
// process from: a Command, which only New builds, and New builds one only
// from a copy of its input that passed every check on the path, argv and
// environment the process will get (forgectl#888).
//
// The package boundary is what makes that a compiler property rather than a
// review one. Command's fields are unexported here, so a composite literal in
// internal/exec cannot set them. A struct type internal/exec declares with the
// same field names is a different type, since an unexported field's identity
// includes its package, so it neither assigns nor converts to a Command. The
// only non-zero Command is therefore one New returned, and New keeps only
// the element copies it checked, in slices of its own, so a write to the
// caller's slices during or after New cannot reach it.
//
// The zero Command has no path, and sealed.Start refuses that on its own.
//
// Go's internal-package rule limits importers to internal/exec and its
// subpackages; TestOnlyExecImportsValidated in internal/exec narrows that to
// internal/exec itself.
package validated

import (
	"errors"
	"fmt"
	"slices"

	"github.com/cameronsjo/forgectl/internal/exec/internal/sealed"
)

// ArgKind separates the three argv element classes the seam recognizes.
type ArgKind uint8

const (
	// ArgUnset is the zero kind: an argument that was never constructed.
	ArgUnset ArgKind = iota
	// ArgFixed is a backend constant, validated at construction.
	ArgFixed
	// ArgOpaque is a dynamic value, accepted as-is because a real path or
	// prompt may contain anything.
	ArgOpaque
	// ArgEndOfOptions is the literal "--" separator.
	ArgEndOfOptions
)

// Arg is one argv element as New checks it.
type Arg struct {
	Value sealed.Value
	Kind  ArgKind
}

// EnvOp is what an environment mutation does to its key.
type EnvOp uint8

const (
	// EnvOpUnspecified is the zero op, which New refuses.
	EnvOpUnspecified EnvOp = iota
	// EnvOpReplace sets the key to Value.
	EnvOpReplace
	// EnvOpUnset removes the key and carries no value.
	EnvOpUnset
)

// KeySopsTmpdir is the one key New restricts by command: it is permitted only
// on the sops edit call, and only with an absolute path. TMPDIR moves where
// sops writes its decrypted copy of a whole document, so a relative one would
// resolve against the child's working directory, which is not the work
// directory it names.
const KeySopsTmpdir = "TMPDIR"

// Env is one permitted change to the inherited environment.
type Env struct {
	Key   string
	Value sealed.Value
	Op    EnvOp
}

// Command is a path, argv and environment mutations that passed New's
// checks. Only New builds a non-zero one.
type Command struct {
	path sealed.Value
	args []sealed.Value
	env  []Env
}

// New checks args and env and returns them as a Command. Each element is read
// once into a local copy, checked, and appended to a slice New owns, so the
// Command holds exactly what was checked.
// sopsEdit reports whether the command is the sops edit call, the only one
// that may set KeySopsTmpdir. Every error is static text: a refusal must not
// become the rendering path that reveals what was wrong with the value.
// sealed answers the questions New asks (whether the path, and a TMPDIR
// value, is absolute; whether an opaque argument leads with a dash) as one
// bit each.
func New(path sealed.Value, args []Arg, env []Env, sopsEdit bool) (Command, error) {
	if !path.Present() {
		return Command{}, errors.New("command path is empty")
	}
	// An absolute path is required so the binary is chosen by the caller and
	// not by exec.LookPath, which reads the live process PATH rather than the
	// runner's captured environment — the one decision where the snapshot
	// would otherwise not apply.
	if !path.IsAbs() {
		return Command{}, errors.New("command path is not absolute")
	}
	seenEndOfOptions := false
	values := make([]sealed.Value, len(args))
	for i, a := range args {
		if !a.Value.Set() || a.Kind == ArgUnset {
			return Command{}, fmt.Errorf("argument %d was never constructed", i)
		}
		values[i] = a.Value
		if a.Kind == ArgEndOfOptions {
			seenEndOfOptions = true
			continue
		}
		if a.Kind == ArgOpaque && !seenEndOfOptions && a.Value.LeadsWithDash() {
			return Command{}, fmt.Errorf("dynamic argument %d begins with a dash and no end-of-options separator precedes it", i)
		}
	}
	seen := make(map[string]struct{}, len(env))
	checked := make([]Env, len(env))
	for i, m := range env {
		if !m.valid() {
			return Command{}, fmt.Errorf("environment mutation %d is not a permitted operation", i)
		}
		if _, dup := seen[m.Key]; dup {
			return Command{}, fmt.Errorf("environment mutation %d duplicates an earlier key", i)
		}
		if m.Key == KeySopsTmpdir {
			if !sopsEdit {
				return Command{}, fmt.Errorf("environment mutation %d is not permitted for this command kind", i)
			}
			if !m.Value.IsAbs() {
				return Command{}, fmt.Errorf("environment mutation %d needs an absolute path", i)
			}
		}
		seen[m.Key] = struct{}{}
		checked[i] = m
	}
	return Command{path: path, args: values, env: checked}, nil
}

// valid requires a replacement value to be non-empty, not merely present.
// Most CLIs treat an empty environment value as unset, so an empty pin would
// silently reopen the auto-discovery window the mutation exists to close,
// while looking like a successful pin in logs that record only the count.
func (m Env) valid() bool {
	switch m.Op {
	case EnvOpReplace:
		return m.Key != "" && m.Value.Present()
	case EnvOpUnset:
		return m.Key != "" && !m.Value.Set()
	default:
		return false
	}
}

// Path is the command's path.
func (c Command) Path() sealed.Value { return c.path }

// Args is a fresh copy of the command's argv, so a caller writing to it
// cannot change the Command.
func (c Command) Args() []sealed.Value { return slices.Clone(c.args) }

// Env is a fresh copy of the command's environment mutations, so a caller
// writing to it cannot change the Command.
func (c Command) Env() []Env { return slices.Clone(c.env) }
