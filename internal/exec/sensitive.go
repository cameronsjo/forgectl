// Sensitive execution seam. The ordinary Runner logs its argv at debug level
// and retains it on *CommandError, which is correct for tmux/sesh/brew
// plumbing and wrong for anything carrying a path, a prompt, a socket, or a
// nonce. This file is the other seam: values go in opaque, are revealed once
// immediately before they fill exec.Cmd, and nothing the runner returns or
// logs can render them back out.
package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// SecretArg is an opaque command path or environment value. Its payload lives
// in an unexported field with no exported accessor: only this package's
// production runner unwraps it, immediately before populating exec.Cmd.
type SecretArg struct {
	payload string
	set     bool
}

// Secret wraps a dynamic value — a path, a socket, a nonce, a prompt — so it
// can travel through adapters without any of them being able to render it.
func Secret(v string) SecretArg { return SecretArg{payload: v, set: true} }

func (SecretArg) String() string                { return Redacted }
func (SecretArg) GoString() string              { return Redacted }
func (SecretArg) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (SecretArg) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (SecretArg) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (SecretArg) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

func (s SecretArg) reveal() string { return s.payload }
func (s SecretArg) present() bool  { return s.set && s.payload != "" }

// Arg is one argv element. Two constructors distinguish a fixed backend
// constant from an opaque dynamic value: the constant is validated at
// construction (it can never legitimately contain a control character or run
// to kilobytes), the dynamic value is accepted as-is because a real path or
// prompt may contain anything. Both render redacted — the runner logs argument
// counts, never argument text, so there is no rendering difference to exploit.
type Arg struct {
	payload string
	secret  bool
	set     bool
}

// Fixed builds an argv element from a backend constant such as "new-session"
// or "-t". It refuses invalid UTF-8, control characters, and oversize input so
// a mistyped constant cannot smuggle a terminal escape into an argv.
func Fixed(v string) (Arg, error) {
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
	return Arg{payload: v, set: true}, nil
}

// MustFixed is Fixed for package-level constants whose validity is a property
// of the source text. It panics on a bad constant, which is a startup failure,
// not a runtime error path.
func MustFixed(v string) Arg {
	a, err := Fixed(v)
	if err != nil {
		panic(err)
	}
	return a
}

// Opaque builds an argv element from a dynamic value: a cwd, a session name, a
// socket path, a ref, a recovery tag, a bootstrap command. Every dynamic value
// reaching a sensitive argv goes through here.
func Opaque(v string) Arg { return Arg{payload: v, secret: true, set: true} }

func (Arg) String() string                { return Redacted }
func (Arg) GoString() string              { return Redacted }
func (Arg) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (Arg) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (Arg) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (Arg) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

func (a Arg) reveal() string { return a.payload }

// Secret reports whether this argument was built from a dynamic value rather
// than a validated backend constant. It exposes the classification, never the
// payload — an adapter fake uses it to assert what a caller passed.
func (a Arg) Secret() bool { return a.secret }

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

// ReplaceHerdrConfigPath pins Herdr to an exact config source.
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

func (m EnvMutation) valid() bool {
	switch m.op {
	case envOpReplace:
		return m.key != "" && m.value.set
	case envOpUnset:
		return m.key != "" && !m.value.set
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
// through the struct and print each field's unexported payload by reflection,
// which is exactly the leak the element types close individually.
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

// validate refuses before process start. Every message here is static text: a
// validation failure must not become the rendering path that reveals what was
// wrong with the value.
func (c SensitiveCommand) validate() error {
	if !c.Kind.Valid() {
		return fmt.Errorf("%w: command kind is not a known operation", ErrInvalidCommand)
	}
	if !c.Path.present() {
		return fmt.Errorf("%w: command path is empty", ErrInvalidCommand)
	}
	for i := range c.Args {
		if !c.Args[i].set {
			return fmt.Errorf("%w: argument %d was never constructed", ErrInvalidCommand, i)
		}
	}
	seen := make(map[string]struct{}, len(c.Env))
	for i := range c.Env {
		m := c.Env[i]
		if !m.valid() {
			return fmt.Errorf("%w: environment mutation %d is not a permitted operation", ErrInvalidCommand, i)
		}
		if _, dup := seen[m.key]; dup {
			return fmt.Errorf("%w: environment mutation %d duplicates an earlier key", ErrInvalidCommand, i)
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
		return fmt.Errorf("%w: %s cap must be positive", ErrInvalidCommand, stream)
	case limit > MaxOutputBytes:
		return fmt.Errorf("%w: %s cap exceeds the %d-byte runner ceiling", ErrInvalidCommand, stream, MaxOutputBytes)
	}
	return nil
}

// BoundedOutput owns at most one stream's cap worth of bytes. It renders as
// byte-count metadata everywhere, and hands out its bytes only through
// CopyBytesForParse, which returns a fresh copy alongside a completeness flag
// the caller cannot ignore — truncated output must never be parsed as a whole
// backend response.
type BoundedOutput struct {
	data     []byte
	overflow bool
}

// Len is the number of bytes retained, which is at most the stream's cap.
func (b BoundedOutput) Len() int { return len(b.data) }

// Complete reports whether the stream ended within its cap. False means the
// process produced more than the cap and the retained bytes are a prefix.
func (b BoundedOutput) Complete() bool { return !b.overflow }

// CopyBytesForParse returns a fresh copy of the retained bytes and whether
// they are the complete stream. The copy keeps a parser from aliasing (and
// mutating) the runner's buffer; the flag is a second return value rather than
// a method so a caller cannot reach the bytes without receiving it.
func (b BoundedOutput) CopyBytesForParse() (data []byte, complete bool) {
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out, !b.overflow
}

func (b BoundedOutput) String() string {
	return "BoundedOutput{bytes:" + strconv.Itoa(len(b.data)) +
		" complete:" + strconv.FormatBool(!b.overflow) + "}"
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
	return slog.GroupValue(slog.Int("bytes", len(b.data)), slog.Bool("complete", !b.overflow))
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
