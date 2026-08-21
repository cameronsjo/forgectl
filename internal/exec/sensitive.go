// Sensitive execution seam. The ordinary Runner logs its argv at debug level
// and retains it on *CommandError, which is correct for tmux, sesh, and brew
// plumbing and wrong for anything carrying a path, a prompt, a socket, or a
// nonce. This file is the other seam: values go in opaque, are revealed once
// immediately before they fill exec.Cmd, and nothing the runner returns or
// logs can render them back out.
//
// # What this seam does not cover
//
// An argv is world-readable to the same user for the lifetime of the process
// (`ps`, /proc). This seam closes the log-file and error-string sinks, which
// is where a prompt or a nonce becomes a durable artifact — it cannot close
// the process table. Do not read Opaque as "safe for a true secret such as an
// API token"; read it as "will not be written down".
//
// Killing is process-scoped, not process-group-scoped, and deliberately so:
// for tmux and cmux the surviving server is the point of the call. A killed
// command's descendants keep running, and keep their own argv in the process
// table; the runner bounds the *call*, not the descendant's lifetime.
package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Redacted is the single fixed public representation of every opaque value in
// this file. It is what %v, %+v, %#v, %q, slog, JSON, and text marshaling all
// produce, so no rendering path has a payload-revealing branch to find.
const Redacted = "[redacted]"

// MaxOutputBytes is the runner-owned hard ceiling on each captured stream.
// A caller-supplied cap may only narrow it; a cap above this refuses before
// process start rather than being silently clamped, so a mistaken cap is a
// loud failure instead of a quiet widening of the bound.
const MaxOutputBytes int64 = 64 << 10

// maxFixedArgBytes bounds a fixed backend constant. Constants are short verb
// and flag spellings; anything approaching this length is a caller mistake
// that should not reach an argv.
const maxFixedArgBytes = 4096

// CommandKind is the closed set of backend operations permitted through the
// sensitive seam. It is the only field of a sensitive command that logging
// records, so it is deliberately a small enum of fixed spellings rather than
// free text an adapter could fill with a path. The zero value is ineligible:
// a command that never set a kind refuses before process start.
type CommandKind uint8

const (
	// KindUnspecified is the ineligible zero value.
	KindUnspecified CommandKind = iota

	KindTmuxReadiness
	KindTmuxSnapshot
	KindTmuxCreate
	KindTmuxReconcile
	KindTmuxProbe
	KindTmuxCleanup

	KindCmuxReadiness
	KindCmuxSnapshot
	KindCmuxCreate
	KindCmuxReconcile
	KindCmuxProbe
	KindCmuxCleanup

	KindHerdrReadiness
	KindHerdrSnapshot
	KindHerdrCreate
	KindHerdrReconcile
	KindHerdrProbe
	KindHerdrCleanup

	kindCount
)

var kindNames = [kindCount]string{
	KindUnspecified: "unspecified",

	KindTmuxReadiness: "tmux.readiness",
	KindTmuxSnapshot:  "tmux.snapshot",
	KindTmuxCreate:    "tmux.create",
	KindTmuxReconcile: "tmux.reconcile",
	KindTmuxProbe:     "tmux.probe",
	KindTmuxCleanup:   "tmux.cleanup",

	KindCmuxReadiness: "cmux.readiness",
	KindCmuxSnapshot:  "cmux.snapshot",
	KindCmuxCreate:    "cmux.create",
	KindCmuxReconcile: "cmux.reconcile",
	KindCmuxProbe:     "cmux.probe",
	KindCmuxCleanup:   "cmux.cleanup",

	KindHerdrReadiness: "herdr.readiness",
	KindHerdrSnapshot:  "herdr.snapshot",
	KindHerdrCreate:    "herdr.create",
	KindHerdrReconcile: "herdr.reconcile",
	KindHerdrProbe:     "herdr.probe",
	KindHerdrCleanup:   "herdr.cleanup",
}

// Valid reports whether k names a real operation. The zero value does not.
func (k CommandKind) Valid() bool { return k > KindUnspecified && k < kindCount }

func (k CommandKind) String() string {
	if k >= kindCount {
		return "invalid(" + strconv.Itoa(int(k)) + ")"
	}
	return kindNames[k]
}

// writeRedacted is the one rendering body behind every Format method here.
// Every verb produces Redacted; %q produces it quoted so a %q consumer still
// receives well-formed output. There is deliberately no verb that reveals.
func writeRedacted(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(Redacted))
		return
	}
	_, _ = io.WriteString(f, Redacted)
}

func redactedJSON() ([]byte, error) { return []byte(strconv.Quote(Redacted)), nil }

// SecretArg is an opaque command path or environment value.
//
// The payload is held in a closure, not a string field, and that is the whole
// containment mechanism rather than a stylistic choice. fmt consults a value's
// Formatter, Stringer, or GoStringer only when reflect.Value.CanInterface()
// reports true, which is false for anything reached through an *unexported*
// field. So a plain string payload here would be printed verbatim by %v, %+v,
// and %#v of any struct that holds this type in an unexported field — the
// natural shape for an adapter client — and slog's TextHandler, which
// production installs, renders a non-TextMarshaler value with exactly
// fmt.Sprintf("%+v", v). A func value prints as an address under every verb at
// every depth, so reflection has nothing to reach.
//
// The cost is that this type is no longer comparable with ==; use Equal.
//
// Every constructor here closes over an immutable string, so reveal is pure
// and repeatable. That is load-bearing, not incidental: validate reveals to
// check the path and the argv, and buildCmd reveals again to fill exec.Cmd.
// A constructor accepting a caller-supplied func would make that pair a
// time-of-check/time-of-use gap while looking like a natural extension.
type SecretArg struct {
	reveal func() string
}

// Secret wraps a dynamic value — a path, a socket, a nonce, a prompt — so it
// can travel through adapters without any of them being able to render it.
func Secret(v string) SecretArg { return SecretArg{reveal: func() string { return v }} }

func (SecretArg) String() string                { return Redacted }
func (SecretArg) GoString() string              { return Redacted }
func (SecretArg) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (SecretArg) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (SecretArg) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (SecretArg) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

// Equal compares two opaque values without revealing either. It replaces ==,
// which the closure payload makes unavailable. Note this is a confirmation
// oracle by construction — a caller who guesses a value can confirm it — which
// is the same trade == offered and is what makes adapter fakes assertable.
func (s SecretArg) Equal(other SecretArg) bool {
	if s.reveal == nil || other.reveal == nil {
		return s.reveal == nil && other.reveal == nil
	}
	return s.reveal() == other.reveal()
}

func (s SecretArg) set() bool { return s.reveal != nil }

func (s SecretArg) present() bool { return s.reveal != nil && s.reveal() != "" }

// argKind separates the three argv element classes the seam recognizes.
type argKind uint8

const (
	argUnset argKind = iota
	// argFixed is a backend constant, validated at construction.
	argFixed
	// argOpaque is a dynamic value, accepted as-is because a real path or
	// prompt may contain anything.
	argOpaque
	// argEndOfOptions is the literal "--" separator.
	argEndOfOptions
)

// Arg is one argv element. Its payload is a closure for the same reason
// SecretArg's is; see that type's comment. Both fixed and opaque arguments
// render redacted — the runner logs argument counts, never argument text, so
// there is no rendering difference for a reader to exploit.
type Arg struct {
	reveal func() string
	kind   argKind
}

// fixed builds an argv element from a backend constant such as "new-session"
// or "-t". It refuses invalid UTF-8, control characters, and oversize input so
// a mistyped constant cannot smuggle a terminal escape into an argv.
//
// It is deliberately unexported: the exempt class is reachable from outside
// this package only through MustFixed, whose parameter type admits constants
// alone. See constantArg for why that is the gate rather than the panic.
func fixed(v string) (Arg, error) {
	switch {
	case v == "":
		return Arg{}, fmt.Errorf("%w: fixed argument is empty", ErrInvalidCommand)
	case len(v) > maxFixedArgBytes:
		return Arg{}, fmt.Errorf("%w: fixed argument exceeds %d bytes", ErrInvalidCommand, maxFixedArgBytes)
	case !utf8.ValidString(v):
		return Arg{}, fmt.Errorf("%w: fixed argument is not valid UTF-8", ErrInvalidCommand)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7F {
			return Arg{}, fmt.Errorf("%w: fixed argument contains a control character", ErrInvalidCommand)
		}
	}
	return Arg{reveal: func() string { return v }, kind: argFixed}, nil
}

// constantArg is the parameter type of MustFixed, and it is what makes "only a
// constant" a compiler rule rather than a comment. An untyped string constant
// is assignable to it, so MustFixed("new-session") compiles anywhere; a value
// of type string is not, and no other package can name this type to perform
// the explicit conversion that would bridge the gap.
//
// That matters because a fixed argument is exempt from the leading-dash refusal
// below — MustFixed("-t") is the whole point — so an exempt class reachable
// from a runtime string is a way to launder a session name or a ref past that
// check. Panicking on a malformed constant does not prevent it: "-rf" is
// well-formed. The type does.
//
// The claim is about the type system, and it stops there: reflect can hand an
// importer this type descriptor without naming it, and Convert will mint one
// from a runtime string. No Go type can close that, and a caller who reaches
// for reflect to get past a constructor is not making a mistake this seam can
// prevent. What the type closes is every accidental route.
type constantArg string

// MustFixed builds an argv element from a backend constant. Its parameter
// accepts only a constant (see constantArg), and it panics on one that is
// malformed, which is a startup failure rather than a runtime error path.
func MustFixed(v constantArg) Arg {
	a, err := fixed(string(v))
	if err != nil {
		panic(err)
	}
	return a
}

// Opaque builds an argv element from a dynamic value: a cwd, a session name, a
// socket path, a ref, a recovery tag, a bootstrap command. Every dynamic value
// reaching a sensitive argv goes through here.
//
// A dynamic value beginning with "-" is an operand the backend would parse as
// a flag, so validate refuses it unless an EndOfOptions separator precedes it.
// That check lives in the seam rather than in each adapter because the seam's
// own redaction is what would make the resulting argv hard to diagnose.
func Opaque(v string) Arg { return Arg{reveal: func() string { return v }, kind: argOpaque} }

// EndOfOptions is the literal "--" separator. Its scope is everything after
// it: once present, no later opaque argument is checked for a leading dash, so
// emit it immediately before the operands rather than early. That the backend
// honours "--" at the specific subcommand is the caller's assertion — the seam
// cannot check it, and a second separator reaches the argv as a literal operand.
func EndOfOptions() Arg {
	return Arg{reveal: func() string { return "--" }, kind: argEndOfOptions}
}

func (Arg) String() string                { return Redacted }
func (Arg) GoString() string              { return Redacted }
func (Arg) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (Arg) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (Arg) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (Arg) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

// Equal compares two arguments without revealing either; see SecretArg.Equal.
func (a Arg) Equal(other Arg) bool {
	if a.kind != other.kind {
		return false
	}
	if a.reveal == nil || other.reveal == nil {
		return a.reveal == nil && other.reveal == nil
	}
	return a.reveal() == other.reveal()
}

// Secret reports whether this argument was built from a dynamic value rather
// than a validated backend constant. It exposes the classification, never the
// payload.
func (a Arg) Secret() bool { return a.kind == argOpaque }

func (a Arg) set() bool { return a.reveal != nil && a.kind != argUnset }

// Environment keys the seam is allowed to touch. There is no constructor that
// takes a key, so an unknown key is unrepresentable rather than rejected.
const (
	envKeyCmuxSocketPath = "CMUX_SOCKET_PATH"
	envKeyCmuxQuiet      = "CMUX_QUIET"
	envKeyHerdrConfig    = "HERDR_CONFIG_PATH"
	envKeyTmux           = "TMUX"
)

type envOp uint8

const (
	envOpUnspecified envOp = iota
	envOpReplace
	envOpUnset
)

// EnvMutation is one permitted change to the inherited environment. The four
// constructors below are the entire vocabulary: a mutation naming any other
// key, or carrying a value on an unset, cannot be constructed at all. Every
// other inherited entry — including a backend CLI's own authentication
// environment — is passed through byte-exact and never inspected or logged.
type EnvMutation struct {
	key   string
	value SecretArg
	op    envOp
}

// ReplaceCmuxSocketPath pins cmux to a resolved endpoint instead of letting it
// auto-discover one after its identity was fingerprinted.
func ReplaceCmuxSocketPath(path string) EnvMutation {
	return EnvMutation{key: envKeyCmuxSocketPath, value: Secret(path), op: envOpReplace}
}

// SetCmuxQuiet sets the fixed CMUX_QUIET=1. Its value is a constant, so it is
// the one mutation whose payload is not caller-supplied.
func SetCmuxQuiet() EnvMutation {
	return EnvMutation{key: envKeyCmuxQuiet, value: Secret("1"), op: envOpReplace}
}

// ReplaceHerdrConfigPath replaces HERDR_CONFIG_PATH.
//
// It does NOT pin herdr to a server, despite what its name and the #181 plan
// both suggest, and an adapter reaching for it as the sanctioned way to pin
// herdr will pin nothing while appearing to. Measured on herdr 0.8.0: a
// HERDR_CONFIG_PATH naming a nonexistent file, and one naming a different
// socket, both resolved to the same endpoint as no config at all. What selects
// a herdr server is the `--session` flag; `herdr session list` maps each
// session name to its own socket.
//
// Kept rather than deleted because it is a permitted mutation of a real
// variable and the seam's own tests use it as a fixture. Whether it should
// survive at all is forgectl#364 — this comment exists so the next reader does
// not have to rediscover what it cannot do.
func ReplaceHerdrConfigPath(path string) EnvMutation {
	return EnvMutation{key: envKeyHerdrConfig, value: Secret(path), op: envOpReplace}
}

// UnsetTmux removes an inherited TMUX so a tmux call cannot be silently
// redirected to whichever server the caller happens to be sitting inside.
func UnsetTmux() EnvMutation {
	return EnvMutation{key: envKeyTmux, op: envOpUnset}
}

func (EnvMutation) String() string                { return Redacted }
func (EnvMutation) GoString() string              { return Redacted }
func (EnvMutation) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (EnvMutation) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (EnvMutation) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (EnvMutation) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

// Equal compares two mutations without revealing either value.
func (m EnvMutation) Equal(other EnvMutation) bool {
	return m.key == other.key && m.op == other.op && m.value.Equal(other.value)
}

// valid requires a replacement value to be non-empty, not merely present. Most
// CLIs treat an empty environment value as unset, so an empty pin would
// silently reopen the auto-discovery window the mutation exists to close —
// while looking like a successful pin in logs that record only the count.
func (m EnvMutation) valid() bool {
	switch m.op {
	case envOpReplace:
		return m.key != "" && m.value.present()
	case envOpUnset:
		return m.key != "" && !m.value.set()
	case envOpUnspecified:
		return false
	default:
		return false
	}
}

// SensitiveCommand is one bounded, redacting invocation. Path and every Args
// element are opaque; Env is drawn from the closed vocabulary above; the caps
// may only narrow the runner-owned ceiling. There is no working-directory
// field on purpose — a cwd is a dynamic value like any other and travels as an
// Opaque argument to whichever backend flag takes it.
type SensitiveCommand struct {
	Kind      CommandKind
	Path      SecretArg
	Args      []Arg
	Env       []EnvMutation
	StdoutCap int64
	StderrCap int64
}

func (c SensitiveCommand) String() string {
	return "SensitiveCommand{kind:" + c.Kind.String() +
		" args:" + strconv.Itoa(len(c.Args)) +
		" env:" + strconv.Itoa(len(c.Env)) +
		" stdout_cap:" + strconv.FormatInt(c.StdoutCap, 10) +
		" stderr_cap:" + strconv.FormatInt(c.StderrCap, 10) + "}"
}

func (c SensitiveCommand) GoString() string { return c.String() }

// Format redacts the aggregate under every verb. Without it, %#v would reach
// through the struct and print each field by reflection.
func (c SensitiveCommand) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(c.String()))
		return
	}
	_, _ = io.WriteString(f, c.String())
}

func (c SensitiveCommand) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("kind", c.Kind.String()),
		slog.Int("args", len(c.Args)),
		slog.Int("env", len(c.Env)),
		slog.Int64("stdout_cap", c.StdoutCap),
		slog.Int64("stderr_cap", c.StderrCap),
	)
}

func (c SensitiveCommand) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(c.String())), nil
}

func (c SensitiveCommand) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// Equal compares two commands without revealing any value — the assertion an
// adapter test makes against a fake's recorded call.
func (c SensitiveCommand) Equal(other SensitiveCommand) bool {
	if c.Kind != other.Kind || c.StdoutCap != other.StdoutCap || c.StderrCap != other.StderrCap {
		return false
	}
	if !c.Path.Equal(other.Path) || len(c.Args) != len(other.Args) || len(c.Env) != len(other.Env) {
		return false
	}
	for i := range c.Args {
		if !c.Args[i].Equal(other.Args[i]) {
			return false
		}
	}
	for i := range c.Env {
		if !c.Env[i].Equal(other.Env[i]) {
			return false
		}
	}
	return true
}

// validate refuses before process start. Every message here is static text: a
// validation failure must not become the rendering path that reveals what was
// wrong with the value. It reveals the path only to test filepath.IsAbs, and
// the argv only to test a leading dash; neither result reaches a message.
func (c SensitiveCommand) validate() error {
	if !c.Kind.Valid() {
		return errors.New("command kind is not a known operation")
	}
	if !c.Path.present() {
		return errors.New("command path is empty")
	}
	// An absolute path is required so the binary is chosen by the caller and
	// not by exec.LookPath, which reads the live process PATH rather than the
	// runner's captured environment — the one decision where the snapshot
	// would otherwise not apply.
	if !filepath.IsAbs(c.Path.reveal()) {
		return errors.New("command path is not absolute")
	}
	seenEndOfOptions := false
	for i := range c.Args {
		a := c.Args[i]
		if !a.set() {
			return fmt.Errorf("argument %d was never constructed", i)
		}
		if a.kind == argEndOfOptions {
			seenEndOfOptions = true
			continue
		}
		if a.kind == argOpaque && !seenEndOfOptions && strings.HasPrefix(a.reveal(), "-") {
			return fmt.Errorf("dynamic argument %d begins with a dash and no end-of-options separator precedes it", i)
		}
	}
	seen := make(map[string]struct{}, len(c.Env))
	for i := range c.Env {
		m := c.Env[i]
		if !m.valid() {
			return fmt.Errorf("environment mutation %d is not a permitted operation", i)
		}
		if _, dup := seen[m.key]; dup {
			return fmt.Errorf("environment mutation %d duplicates an earlier key", i)
		}
		seen[m.key] = struct{}{}
	}
	if err := validCap("stdout", c.StdoutCap); err != nil {
		return err
	}
	return validCap("stderr", c.StderrCap)
}

func validCap(stream string, limit int64) error {
	switch {
	case limit <= 0:
		return fmt.Errorf("%s cap must be positive", stream)
	case limit > MaxOutputBytes:
		return fmt.Errorf("%s cap exceeds the %d-byte runner ceiling", stream, MaxOutputBytes)
	}
	return nil
}

// outputBuf holds captured bytes behind a pointer so that a BoundedOutput
// reached through an unexported field renders as an address rather than as the
// decimal byte dump reflection would otherwise produce. Same containment
// reasoning as SecretArg's closure.
//
// Both the type and the field are unexported, so no importer can reach them.
// Inside this package they can: %#v on a bare *outputBuf dumps the bytes, so
// never hand one to slog or fmt directly — log the BoundedOutput.
type outputBuf struct{ data []byte }

// BoundedOutput owns at most one stream's cap worth of bytes. It renders as
// byte-count metadata everywhere, and hands out its bytes only through
// CopyBytesForParse, which returns a fresh copy alongside a completeness flag
// the caller cannot ignore — a partial stream must never be parsed as a whole
// backend response.
type BoundedOutput struct {
	buf *outputBuf

	// overflow means the stream produced more than its cap.
	overflow bool
	// forced means the read end was retired before the stream reached EOF —
	// after a kill, or after the retirement bound expired with a descendant
	// still holding the write end. The bytes are a prefix either way, but the
	// two causes are different answers to "should I retry".
	forced bool
}

// Len is the number of bytes retained, which is at most the stream's cap.
func (b BoundedOutput) Len() int {
	if b.buf == nil {
		return 0
	}
	return len(b.buf.data)
}

// Complete reports whether the stream was read to EOF within its cap. False
// means the retained bytes are a prefix — because the stream exceeded the cap,
// or because the read end was retired before EOF.
//
// A successful run can report false, and for the backends this seam exists to
// drive that is the expected case rather than an anomaly: a daemon the command
// spawned inherits the write end and outlives the command, so the runner
// retires the pipe on its own schedule. That is why a cut-off stream is not an
// error — the command did what it was asked — and why a caller that parses
// output must read this flag rather than the returned error.
func (b BoundedOutput) Complete() bool { return !b.overflow && !b.forced }

// CopyBytesForParse returns a fresh copy of the retained bytes and whether
// they are the complete stream. The copy keeps a parser from aliasing (and
// mutating) the runner's buffer; the flag is a second return value rather than
// a method so a caller cannot reach the bytes without receiving it.
func (b BoundedOutput) CopyBytesForParse() (data []byte, complete bool) {
	out := make([]byte, b.Len())
	if b.buf != nil {
		copy(out, b.buf.data)
	}
	return out, b.Complete()
}

func (b BoundedOutput) String() string {
	return "BoundedOutput{bytes:" + strconv.Itoa(b.Len()) +
		" complete:" + strconv.FormatBool(b.Complete()) + "}"
}

func (b BoundedOutput) GoString() string { return b.String() }

func (b BoundedOutput) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(b.String()))
		return
	}
	_, _ = io.WriteString(f, b.String())
}

func (b BoundedOutput) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("bytes", b.Len()), slog.Bool("complete", b.Complete()))
}

func (b BoundedOutput) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(b.String())), nil
}

func (b BoundedOutput) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// SensitiveResult is what a sensitive command produced. It is returned
// alongside a typed error on every non-success outcome too, so an adapter can
// classify a bounded backend response without the runner having to decide in
// advance which failures carry useful output.
type SensitiveResult struct {
	Stdout   BoundedOutput
	Stderr   BoundedOutput
	ExitCode int
}

func (r SensitiveResult) String() string {
	return "SensitiveResult{exit:" + strconv.Itoa(r.ExitCode) +
		" stdout:" + r.Stdout.String() + " stderr:" + r.Stderr.String() + "}"
}

func (r SensitiveResult) GoString() string { return r.String() }

func (r SensitiveResult) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(r.String()))
		return
	}
	_, _ = io.WriteString(f, r.String())
}

func (r SensitiveResult) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("exit", r.ExitCode),
		slog.Any("stdout", r.Stdout),
		slog.Any("stderr", r.Stderr),
	)
}

func (r SensitiveResult) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(r.String())), nil
}

func (r SensitiveResult) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// Outcome is the closed classification of how a sensitive command ended. The
// distinctions matter to callers: a timeout and an output-limit kill both look
// like "the process died young" but mean different things about the backend.
type Outcome uint8

const (
	// OutcomeUnspecified is the ineligible zero value.
	OutcomeUnspecified Outcome = iota
	// OutcomeInvalid means the command was refused before process start.
	OutcomeInvalid
	// OutcomeStartFailed means fork/exec never succeeded.
	OutcomeStartFailed
	// OutcomeExit means the process ran and exited nonzero.
	OutcomeExit
	// OutcomeTimeout means the context deadline expired and the process was killed.
	OutcomeTimeout
	// OutcomeCanceled means the context was canceled and the process was killed.
	OutcomeCanceled
	// OutcomeOutputLimit means a stream exceeded its cap and the process was killed.
	OutcomeOutputLimit

	outcomeCount
)

var outcomeNames = [outcomeCount]string{
	OutcomeUnspecified: "unspecified",
	OutcomeInvalid:     "invalid",
	OutcomeStartFailed: "start_failed",
	OutcomeExit:        "exit",
	OutcomeTimeout:     "timeout",
	OutcomeCanceled:    "canceled",
	OutcomeOutputLimit: "output_limit",
}

func (o Outcome) String() string {
	if o >= outcomeCount {
		return "invalid(" + strconv.Itoa(int(o)) + ")"
	}
	return outcomeNames[o]
}

// Sentinels for errors.Is. A *SensitiveError unwraps to exactly one of these
// and never to the underlying os/exec error, whose text can contain the path
// that failed to start.
var (
	ErrInvalidCommand = errors.New("exec: sensitive command refused before start")
	ErrStartFailed    = errors.New("exec: sensitive command failed to start")
	ErrNonzeroExit    = errors.New("exec: sensitive command exited nonzero")
	ErrTimeout        = errors.New("exec: sensitive command timed out")
	ErrCanceled       = errors.New("exec: sensitive command canceled")
	ErrOutputLimit    = errors.New("exec: sensitive command exceeded its output ceiling")
)

var outcomeSentinels = [outcomeCount]error{
	OutcomeInvalid:     ErrInvalidCommand,
	OutcomeStartFailed: ErrStartFailed,
	OutcomeExit:        ErrNonzeroExit,
	OutcomeTimeout:     ErrTimeout,
	OutcomeCanceled:    ErrCanceled,
	OutcomeOutputLimit: ErrOutputLimit,
}

// SensitiveError is the only error type this seam returns. Every field is
// metadata: a closed kind, a closed outcome, an exit code, and byte counts.
// It deliberately holds no underlying error — the os/exec error for a failed
// start embeds the path, so wrapping it would reopen through errors.Unwrap and
// %v exactly the leak the opaque types close. reason is static text chosen
// from this package, never a caller value.
type SensitiveError struct {
	Kind        CommandKind
	Outcome     Outcome
	ExitCode    int
	StdoutBytes int
	StderrBytes int

	reason string
}

func (e *SensitiveError) Error() string {
	var b strings.Builder
	b.WriteString("sensitive command ")
	b.WriteString(e.Kind.String())
	b.WriteString(": ")
	b.WriteString(e.Outcome.String())
	if e.reason != "" {
		b.WriteString(" (")
		b.WriteString(e.reason)
		b.WriteString(")")
	}
	b.WriteString(" [exit=")
	b.WriteString(strconv.Itoa(e.ExitCode))
	b.WriteString(" stdout=")
	b.WriteString(strconv.Itoa(e.StdoutBytes))
	b.WriteString(" stderr=")
	b.WriteString(strconv.Itoa(e.StderrBytes))
	b.WriteString("]")
	return b.String()
}

// Format and GoString are armor, not decoration. Every field here is metadata
// today, so %#v is safe by inspection — but by-inspection safety expires the
// day someone adds a payload-bearing field, and it expires silently. These two
// methods make the invariant structural instead.
func (e *SensitiveError) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(e.Error()))
		return
	}
	_, _ = io.WriteString(f, e.Error())
}

func (e *SensitiveError) GoString() string { return e.Error() }

// Unwrap returns the outcome's sentinel, not the underlying process error, so
// errors.Is works while errors.Unwrap cannot reach a payload-bearing error.
func (e *SensitiveError) Unwrap() error {
	if e.Outcome >= outcomeCount {
		return nil
	}
	return outcomeSentinels[e.Outcome]
}

func (e *SensitiveError) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("kind", e.Kind.String()),
		slog.String("outcome", e.Outcome.String()),
		slog.Int("exit", e.ExitCode),
		slog.Int("stdout_bytes", e.StdoutBytes),
		slog.Int("stderr_bytes", e.StderrBytes),
	)
}

func (e *SensitiveError) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(e.Error())), nil
}

func (e *SensitiveError) MarshalText() ([]byte, error) { return []byte(e.Error()), nil }

// newSensitiveError builds the error for an outcome. reason must be a literal
// from this package; it is the one free-text field and it never carries a
// caller value.
func newSensitiveError(kind CommandKind, outcome Outcome, res SensitiveResult, reason string) *SensitiveError {
	return &SensitiveError{
		Kind:        kind,
		Outcome:     outcome,
		ExitCode:    res.ExitCode,
		StdoutBytes: res.Stdout.Len(),
		StderrBytes: res.Stderr.Len(),
		reason:      reason,
	}
}

// SensitiveRunner runs a bounded, redacting command. It is deliberately a
// second interface rather than more methods on Runner: widening Runner would
// let an ordinary caller route a bootstrap-bearing command through the
// argv-logging path by accident, which is the failure this seam exists to make
// impossible.
type SensitiveRunner interface {
	RunSensitive(ctx context.Context, cmd SensitiveCommand) (SensitiveResult, error)
}
