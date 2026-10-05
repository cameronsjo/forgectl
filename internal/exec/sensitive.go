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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/exec/internal/sealed"
	"github.com/cameronsjo/forgectl/internal/exec/internal/validated"
	"github.com/cameronsjo/forgectl/internal/redact"
)

// Redacted is the single fixed public representation of every opaque value in
// this file. It is what %v, %+v, %#v, %q, slog, JSON, and text marshaling all
// produce, so no rendering path has a payload-revealing branch to find.
const Redacted = redact.Marker

// MaxOutputBytes is the runner-owned hard ceiling on each captured stream.
// A caller-supplied cap may only narrow it; a cap above this refuses before
// process start rather than being silently clamped, so a mistaken cap is a
// loud failure instead of a quiet widening of the bound.
const MaxOutputBytes int64 = 64 << 10

// CaptureMode selects how stdout is retained. The zero mode preserves the raw
// bounded capture every existing caller uses. CaptureCmuxWorkspaceList is a
// closed, schema-specific projection for cmux's workspace-list envelope: the
// runner streams past fields the adapter never reads and retains only workspace
// id and description values under the same output ceiling.
type CaptureMode uint8

const (
	CaptureRaw CaptureMode = iota
	CaptureCmuxWorkspaceList
	captureModeCount
)

func (m CaptureMode) valid() bool { return m < captureModeCount }

func (m CaptureMode) String() string {
	switch m {
	case CaptureRaw:
		return "raw"
	case CaptureCmuxWorkspaceList:
		return "cmux_workspace_list"
	default:
		return "invalid(" + strconv.Itoa(int(m)) + ")"
	}
}

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
	// KindHerdrPaneInspect reads a pane's foreground process before anything is
	// typed into it. KindHerdrBootstrap is the `pane run` that types the
	// trampoline line into a pane the inspection found idle.
	KindHerdrPaneInspect
	KindHerdrBootstrap
	// KindHerdrNotify is `notification show`: a desktop notification whose
	// title and body are item names and WHAT text, which is why it routes
	// through this seam rather than the argv-logging Runner.
	KindHerdrNotify
	// KindHerdrPaneSplit, KindHerdrPaneRename and KindHerdrPaneRun build the desk
	// layout. pane run types an operator-built command into a pane, and a
	// split carries a cwd, so these stay off the argv-logging Runner too.
	KindHerdrPaneSplit
	KindHerdrPaneRename
	KindHerdrPaneRun
	// KindHerdrScreenRead and KindHerdrPaneStatus are `surface ready`'s two
	// reads of a worker's root pane: its visible text and herdr's agent status.
	KindHerdrScreenRead
	KindHerdrPaneStatus

	// KindSopsEdit drives `sops <file>` with forgectl re-invoked as the
	// editor. KindSopsExtract is the read-back that proves what landed.
	//
	// These route through the sensitive seam rather than Runner for a reason
	// the ordinary path cannot satisfy: sops' stderr quotes the offending
	// line of the document it failed to parse, and that line is
	// `key: '<the secret>'`. Runner's runAndWrap logs stderr at Error level —
	// recorded at every enabled log_level (log_level defaults to off), and it
	// can be pointed at a file on disk — and retains it on *CommandError,
	// which fang renders. Here,
	// nothing logged or returned can render a payload, and both streams are
	// capped so the measured 8.4 MB of sops re-invocation stderr cannot grow
	// the heap.
	KindSopsEdit
	KindSopsExtract

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

	KindHerdrReadiness:   "herdr.readiness",
	KindHerdrSnapshot:    "herdr.snapshot",
	KindHerdrCreate:      "herdr.create",
	KindHerdrReconcile:   "herdr.reconcile",
	KindHerdrProbe:       "herdr.probe",
	KindHerdrCleanup:     "herdr.cleanup",
	KindHerdrPaneInspect: "herdr.pane-inspect",
	KindHerdrBootstrap:   "herdr.bootstrap",
	KindHerdrNotify:      "herdr.notification-show",
	KindHerdrPaneSplit:   "herdr.pane-split",
	KindHerdrPaneRename:  "herdr.pane-rename",
	KindHerdrPaneRun:     "herdr.pane-run",
	KindHerdrScreenRead:  "herdr.screen-read",
	KindHerdrPaneStatus:  "herdr.pane-status",

	KindSopsEdit:    "sops.edit",
	KindSopsExtract: "sops.extract",
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
// The payload lives in a sealed.Value (internal/exec/internal/sealed), and
// that package is the only code that can read it: this one cannot either
// (forgectl#854). sealed.Value holds the payload in a closure, not a string
// field, and that is the containment mechanism against rendering rather than a
// stylistic choice. fmt consults a value's Formatter, Stringer, or GoStringer
// only when reflect.Value.CanInterface() reports true, which is false for
// anything reached through an *unexported* field. So a plain string payload
// would be printed verbatim by %v, %+v, and %#v of any struct that holds this
// type in an unexported field — the natural shape for an adapter client — and
// slog's TextHandler, which production installs, renders a non-TextMarshaler
// value with exactly fmt.Sprintf("%+v", v). A func value prints as an address
// under every verb at every depth, so fmt's reflection has nothing to print.
//
// The cost is that this type is no longer comparable with ==; use Equal.
//
// Every constructor here seals an immutable string, so a reveal is pure and
// repeatable. That is load-bearing, not incidental: validate checks the path
// and the argv through sealed's predicates, and sealed.Start reveals again to
// start the process. A constructor accepting a caller-supplied
// func would make that pair a time-of-check/time-of-use gap while looking like
// a natural extension.
type SecretArg struct {
	v sealed.Value
}

// Secret wraps a dynamic value — a path, a socket, a nonce, a prompt — so it
// can travel through adapters without any of them being able to render it.
func Secret(v string) SecretArg { return SecretArg{v: sealed.New(v)} }

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
func (s SecretArg) Equal(other SecretArg) bool { return s.v.Equal(other.v) }

// argKind separates the three argv element classes the seam recognizes. It is
// validated.ArgKind, which New checks, so the two cannot drift.
type argKind = validated.ArgKind

const (
	argUnset        = validated.ArgUnset
	argFixed        = validated.ArgFixed
	argOpaque       = validated.ArgOpaque
	argEndOfOptions = validated.ArgEndOfOptions
)

// Arg is one argv element. Its payload is a sealed.Value for the same reason
// SecretArg's is; see that type's comment. Both fixed and opaque arguments
// render redacted — the runner logs argument counts, never argument text, so
// there is no rendering difference for a reader to exploit.
type Arg struct {
	v    sealed.Value
	kind argKind
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
	return Arg{v: sealed.New(v), kind: argFixed}, nil
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
func Opaque(v string) Arg { return Arg{v: sealed.New(v), kind: argOpaque} }

// EndOfOptions is the literal "--" separator. Its scope is everything after
// it: once present, no later opaque argument is checked for a leading dash, so
// emit it immediately before the operands rather than early. That the backend
// honours "--" at the specific subcommand is the caller's assertion — the seam
// cannot check it, and a second separator reaches the argv as a literal operand.
func EndOfOptions() Arg {
	return Arg{v: sealed.New("--"), kind: argEndOfOptions}
}

func (Arg) String() string                { return Redacted }
func (Arg) GoString() string              { return Redacted }
func (Arg) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (Arg) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (Arg) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (Arg) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

// Equal compares two arguments without revealing either; see SecretArg.Equal.
func (a Arg) Equal(other Arg) bool {
	return a.kind == other.kind && a.v.Equal(other.v)
}

// Secret reports whether this argument was built from a dynamic value rather
// than a validated backend constant. It exposes the classification, never the
// payload.
func (a Arg) Secret() bool { return a.kind == argOpaque }

func (a Arg) set() bool { return a.v.Set() && a.kind != argUnset }

// Environment keys the seam is allowed to touch. There is no constructor that
// takes a key, so an unknown key is unrepresentable rather than rejected.
const (
	envKeyCmuxSocketPath = "CMUX_SOCKET_PATH"
	envKeyCmuxQuiet      = "CMUX_QUIET"
	envKeyHerdrConfig    = "HERDR_CONFIG_PATH"
	envKeyTmux           = "TMUX"

	// The sops editor protocol. Three variables, not five: the work directory
	// is named once and the value file, the result file, the nonce file, and
	// the invocation counter all sit at fixed names inside it. Every name
	// added here is a name an attacker could try to set, so the smaller
	// surface is the point.
	//
	// Note what is NOT here: the value. Its containing directory's path
	// travels; the secret itself never enters an environment, which is
	// readable from /proc on Linux for the lifetime of the process.
	envKeySopsEditor = "EDITOR"

	// envKeySopsTmpdir is where sops puts the decrypted copy of the WHOLE
	// document that its editor edits. It is not part of the editor protocol:
	// it confines that copy to forgectl's work directory. See
	// ReplaceSopsTmpdir.
	envKeySopsTmpdir = validated.KeySopsTmpdir
)

// The sops editor protocol's variable names, EXPORTED so the reading side
// (internal/cli's `__sops-edit`) references these rather than keeping its own
// copies.
//
// They were spelled twice, in two packages, with a comment on the other side
// describing itself as a mirror. Renaming one side compiled clean, passed
// every unit test, and broke only the real subprocess — which is covered
// exclusively by gated integration tests. A shared constant prevents the
// drift; a test asserting two literals are equal would only have detected it.
const (
	EnvSopsWorkdir = "FORGECTL_SOPS_WORKDIR"
	EnvSopsPath    = "FORGECTL_SOPS_PATH"
	EnvSopsNonce   = "FORGECTL_SOPS_NONCE"
)

// envOp is validated.EnvOp, which New checks, so the two cannot drift.
type envOp = validated.EnvOp

const (
	envOpReplace = validated.EnvOpReplace
	envOpUnset   = validated.EnvOpUnset
)

// EnvMutation is one permitted change to the inherited environment. The
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

// ReplaceSopsEditor points sops' EDITOR at a command. sops shell-word-splits
// the value (quotes honoured) and appends the decrypted temp file as the only
// argument — measured on 3.13.3, including the self-exec case.
func ReplaceSopsEditor(command string) EnvMutation {
	return EnvMutation{key: envKeySopsEditor, value: Secret(command), op: envOpReplace}
}

// ReplaceSopsTmpdir points sops' temp directory at path, which the driver
// sets to its own work directory.
//
// `sops edit` decrypts the whole document into a file under os.TempDir while
// its editor runs. Measured on 3.13.3 (cameronsjo/forgectl#560): SIGINT and
// SIGTERM make sops remove it, but SIGHUP and SIGQUIT leave it, plaintext and
// whole, and SIGHUP is what closing the terminal sends to forgectl and sops
// alike. Inside the work directory, the plaintext guard removes it on those
// signals, and after an uncatchable one the leftover scan refuses on the
// directory that holds it. os.TempDir reads TMPDIR on unix only, so this
// confines nothing on Windows.
func ReplaceSopsTmpdir(path string) EnvMutation {
	return EnvMutation{key: envKeySopsTmpdir, value: Secret(path), op: envOpReplace}
}

// ReplaceSopsWorkdir names the private directory holding the value file, the
// nonce, the result, and the invocation counter.
func ReplaceSopsWorkdir(path string) EnvMutation {
	return EnvMutation{key: EnvSopsWorkdir, value: Secret(path), op: envOpReplace}
}

// ReplaceSopsPath carries the dotted key path the editor must write.
func ReplaceSopsPath(path string) EnvMutation {
	return EnvMutation{key: EnvSopsPath, value: Secret(path), op: envOpReplace}
}

// ReplaceSopsNonce carries the per-run nonce the editor checks against the
// copy in its work directory.
//
// It bounds a STRAY invocation — sops re-running the editor after a run's
// files are gone, a replay from a stale environment, a hand-typed call that
// forgot the protocol. It is NOT a privilege boundary, and an earlier version
// of this comment claimed it was: a caller who can set this process's
// environment can also create the directory and nonce file it names, so the
// nonce buys nothing against them. It does not need to, either — a caller who
// can exec forgectl can already write YAML with a shell. The full reasoning
// is on internal/cli's newSopsEditCmd, and this comment used to contradict it.
func ReplaceSopsNonce(nonce string) EnvMutation {
	return EnvMutation{key: EnvSopsNonce, value: Secret(nonce), op: envOpReplace}
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

// SensitiveCommand is one bounded, redacting invocation. Path and every Args
// element are opaque; Env is drawn from the closed vocabulary above; the caps
// may only narrow the runner-owned ceiling. There is no working-directory
// field on purpose — a cwd is a dynamic value like any other and travels as an
// Opaque argument to whichever backend flag takes it.
type SensitiveCommand struct {
	Kind       CommandKind
	Path       SecretArg
	Args       []Arg
	Env        []EnvMutation
	StdoutMode CaptureMode
	StdoutCap  int64
	StderrCap  int64
}

func (c SensitiveCommand) String() string {
	return "SensitiveCommand{kind:" + c.Kind.String() +
		" args:" + strconv.Itoa(len(c.Args)) +
		" env:" + strconv.Itoa(len(c.Env)) +
		" stdout_mode:" + c.StdoutMode.String() +
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
		slog.String("stdout_mode", c.StdoutMode.String()),
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
	if c.Kind != other.Kind || c.StdoutMode != other.StdoutMode || c.StdoutCap != other.StdoutCap || c.StderrCap != other.StderrCap {
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

// toValidated is m as validated.New checks it.
func (m EnvMutation) toValidated() validated.Env {
	return validated.Env{Key: m.key, Value: m.value.v, Op: m.op}
}

// validate refuses before process start; see validated.
func (c SensitiveCommand) validate() error {
	_, err := c.validated()
	return err
}

// validated checks c and returns the part that reaches a process (its path,
// argv and environment mutations) as a validated.Command, the only thing
// startSealed accepts. The checks on that part run in validated.New, over a
// copy it takes first, so the command started is the command checked, by
// construction: a write to c's Args or Env backing arrays afterwards cannot
// reach it, and no code here can build a Command any other way
// (forgectl#888). The kind, capture mode and caps, which shape how the runner
// reads the process rather than what the process gets, are checked here.
//
// Every message is static text: a validation failure must not become the
// rendering path that reveals what was wrong with the value.
func (c SensitiveCommand) validated() (validated.Command, error) {
	if !c.Kind.Valid() {
		return validated.Command{}, errors.New("command kind is not a known operation")
	}
	if !c.StdoutMode.valid() {
		return validated.Command{}, errors.New("stdout capture mode is not supported")
	}
	args := make([]validated.Arg, len(c.Args))
	for i, a := range c.Args {
		args[i] = validated.Arg{Value: a.v, Kind: a.kind}
	}
	env := make([]validated.Env, len(c.Env))
	for i, m := range c.Env {
		env[i] = m.toValidated()
	}
	cmd, err := validated.New(c.Path.v, args, env, c.Kind == KindSopsEdit)
	if err != nil {
		return validated.Command{}, err
	}
	if err := validCap("stdout", c.StdoutCap); err != nil {
		return validated.Command{}, err
	}
	if err := validCap("stderr", c.StderrCap); err != nil {
		return validated.Command{}, err
	}
	return cmd, nil
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

// outputBuf holds captured bytes behind a pointer, and the bytes themselves
// behind a closure, so that a BoundedOutput reached through an unexported
// field renders as an address rather than as the decimal byte dump
// reflection would otherwise produce. Same containment reasoning as
// SecretArg's closure.
//
// The closure is also what keeps the bytes from reflect's plain-data readers
// (forgectl#897): Value.Bytes, Index and Uint read an unexported []byte field
// without the read-only check, so a field would hand the bytes to any code
// holding a BoundedOutput, past CopyBytesForParse. A func value's captures are
// no field reflect can walk into. read is called only by CopyBytesForParse
// (TestOnlyCopyBytesForParseReadsOutput); Len reads n.
type outputBuf struct {
	n    int
	read func() []byte
}

// newOutputBuf seals data, which must not be modified afterwards.
func newOutputBuf(data []byte) *outputBuf {
	return &outputBuf{n: len(data), read: func() []byte { return data }}
}

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
	return b.buf.n
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
		copy(out, b.buf.read())
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
