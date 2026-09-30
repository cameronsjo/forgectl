// Package sealed holds internal/exec's opaque payloads and the only code that
// reads them (forgectl#854).
//
// The containment is a compiler rule rather than a review rule. The payload
// sits behind an unexported field of Value, so no package but this one can
// read it: internal/exec included. Go's internal-package rule then limits who
// can import this package at all to internal/exec and its subpackages. Nothing
// exported here returns a payload as a string, and nothing takes a callback,
// so a reveal written anywhere else does not compile.
//
// The one way a payload leaves is Start, which puts it into a child process's
// path, argv and environment and hands back a *Proc that can only wait for or
// kill that process. The *exec.Cmd holding the plaintext is built and kept
// inside this package; nothing exported returns it, and Proc holds only
// closures over it, which reflection cannot read. Everything else is a fixed
// one-bit predicate (Set, Present, IsAbs, LeadsWithDash), a comparison
// (Equal), or a re-spelling over the closed Transform set that yields another
// sealed Value (Map).
//
// Two things the package boundary does not stop, and the guard tests in
// internal/exec still refuse: a //go:linkname directive, which binds to any
// symbol in any package, internal or not, and an "unsafe" import, which reads
// any memory. The exported surface below is pinned in internal/exec's API
// golden, so a new export here fails until reviewed.
package sealed

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/tmux/tmuxesc"
)

// Value is one sealed payload: a path, a socket, a nonce, a prompt, an argv
// element, or an environment value.
//
// The payload is held in a closure, not a string field, and that is the
// containment mechanism against fmt and slog rather than a stylistic choice.
// fmt consults a value's Formatter, Stringer, or GoStringer only when
// reflect.Value.CanInterface() reports true, which is false for anything
// reached through an unexported field. So a plain string payload here would be
// printed verbatim by %v, %+v, and %#v of any struct that holds a Value in an
// unexported field, which is exactly how internal/exec holds it. A func value
// prints as an address under every verb at every depth, so reflection has
// nothing to reach.
//
// Every constructor closes over an immutable string, so reveal is pure and
// repeatable. That is load-bearing: validation reads the payload to check it,
// and Command reads it again to fill exec.Cmd. A constructor accepting a
// caller-supplied func would make that pair a time-of-check/time-of-use gap.
type Value struct {
	reveal func() string
}

// New seals v.
func New(v string) Value { return Value{reveal: func() string { return v }} }

// Set reports whether v was built by New (or Map) rather than being the zero
// Value.
func (v Value) Set() bool { return v.reveal != nil }

// Present reports whether v is set and non-empty.
func (v Value) Present() bool { return v.reveal != nil && v.reveal() != "" }

// IsAbs reports whether v is set and an absolute path (filepath.IsAbs).
func (v Value) IsAbs() bool { return v.reveal != nil && filepath.IsAbs(v.reveal()) }

// LeadsWithDash reports whether v is set and begins with "-", which a backend
// would parse as a flag.
func (v Value) LeadsWithDash() bool { return v.reveal != nil && strings.HasPrefix(v.reveal(), "-") }

// Equal compares two values without revealing either. Two zero Values are
// equal; a zero Value equals nothing else. It is a confirmation oracle by
// construction (a caller who guesses a value can confirm it), which is the
// same trade == on a string offered.
func (v Value) Equal(other Value) bool {
	if v.reveal == nil || other.reveal == nil {
		return v.reveal == nil && other.reveal == nil
	}
	return v.reveal() == other.reveal()
}

// Transform is a re-spelling applied to a sealed payload, for a backend that
// reads the value in its own syntax: tmux format-expands a -c directory, so
// the tmux adapter must escape its sealed cwd (forgectl#839).
//
// The set is CLOSED by the compiler. apply is unexported, so only a
// constructor in this package can set it, and every constructor here names a
// fixed, pure escape from the leaf package tmuxesc. A conversion from an
// identical struct declared elsewhere does not compile either, because an
// unexported field name from another package is a different field.
type Transform struct {
	apply func(string) string
}

// TmuxDirOperand escapes a directory for a tmux -c operand: format expansion
// and the trailing-';' command split (tmuxesc.DirOperand).
func TmuxDirOperand() Transform { return Transform{apply: tmuxesc.DirOperand} }

// Map returns a new Value holding t applied to v's payload. t runs ONCE, here,
// and the result is closed over as an immutable string, so the new Value's
// reveal stays pure and repeatable. A zero v or a zero t maps to the zero
// Value.
func (v Value) Map(t Transform) Value {
	if v.reveal == nil || t.apply == nil {
		return Value{}
	}
	return New(t.apply(v.reveal()))
}

// EnvVar is one environment entry Start appends as Key=Value. The key is not
// a payload: internal/exec draws it from a closed set of constants.
type EnvVar struct {
	Key   string
	Value Value
}

// errNotStarted is the only error Start returns. The os/exec error for a
// failed start names the path, so it is dropped here rather than handed to a
// caller that could render it.
var errNotStarted = errors.New("sealed: process did not start")

// Proc is a started child process. It offers what the runner needs to bound
// the call, waiting and killing, and nothing that reads the path, argv or
// environment it was started with.
//
// Both fields are closures over the unexported *exec.Cmd rather than the Cmd
// itself. A field holding the Cmd would be readable through reflection by
// its string-kind accessors (reflect.Value.String works on an unexported
// field; only Interface is refused), and that would reach Path and Args. A
// captured variable is out of reflection's reach, the same containment Value
// uses.
type Proc struct {
	wait func() error
	kill func() error
}

// Wait waits for the process to exit and returns exec.Cmd.Wait's error. That
// error is nil, an *exec.ExitError (which carries the wait status and resource
// usage, never the path or argv), or one of os/exec's fixed-text misuse
// errors; with *os.File streams no copying goroutine exists to add another.
func (p *Proc) Wait() error { return p.wait() }

// Kill kills the process (os.Process.Kill); it does not wait for it.
func (p *Proc) Kill() error { return p.kill() }

// Start is the reveal boundary: the only place any payload leaves its wrapper,
// and it leaves into a child process and nowhere else. It builds the command
// (see command), connects stdout and stderr, and starts it. On failure it
// returns errNotStarted and never the os/exec error, whose text names the
// path. Every Value must be set; the caller validates before calling.
//
// It takes *os.File streams rather than io.Writer so exec.Cmd starts no
// copying goroutine: the runner owns reading, capping and retiring both pipes.
// It starts no context watcher either, because the runner owns killing and
// reaping in a defined order (see OSSensitiveRunner.RunSensitive), and path
// must already be absolute so no PATH lookup happens.
func Start(path Value, args []Value, env []string, set []EnvVar, stdout, stderr *os.File) (*Proc, error) {
	cmd := command(path, args, env, set)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, errNotStarted
	}
	return &Proc{
		wait: cmd.Wait,
		kill: func() error { return cmd.Process.Kill() },
	}, nil
}

// command builds the *exec.Cmd Start runs: Path and Args from path and args,
// and Env set to a fresh copy of env followed by one Key=Value entry per
// element of set, in order. Env is never nil, so the child never silently
// inherits the live process environment. It is unexported, and Start never
// hands its result out, so the plaintext it assembles exists only here.
func command(path Value, args []Value, env []string, set []EnvVar) *exec.Cmd {
	argv := make([]string, len(args))
	for i := range args {
		argv[i] = args[i].reveal()
	}
	cmd := exec.Command(path.reveal(), argv...) //nolint:gosec,noctx // G204: the sealed seam's one reveal, validated absolute by the caller; noctx: the runner owns kill and reap, so CommandContext would race it (see OSSensitiveRunner.RunSensitive)
	out := make([]string, 0, len(env)+len(set))
	out = append(out, env...)
	for _, e := range set {
		out = append(out, e.Key+"="+e.Value.reveal())
	}
	cmd.Env = out
	return cmd
}
