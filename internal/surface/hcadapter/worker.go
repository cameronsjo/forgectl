package hcadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// screenLines is how many history rows above the screen `hc read` returns,
// so a REPORT line that scrolled off the visible screen is still found.
const screenLines = "200"

var (
	// ErrWorkerGone reports a worker whose terminal no longer exists on the
	// core its reference was bound to.
	ErrWorkerGone = errors.New("hc: the worker's terminal is gone")
	// ErrUnreadable reports a read that cannot say what the worker shows. It
	// is never a verdict about the worker.
	ErrUnreadable = errors.New("hc: the worker's terminal could not be read")
	// ErrSendRefused reports a send hc refused before typing anything: the
	// worker is blocked, finished, or not in the expected status.
	ErrSendRefused = errors.New("hc: the send was refused and nothing was typed")
	// ErrSendFailed reports a send whose outcome is unknown: the text may
	// have arrived.
	ErrSendFailed = errors.New("hc: the send failed, and whether it reached the worker is unknown")
)

// Worker is one read of a worker's terminal.
type Worker struct {
	// Live is false once the worker's program has exited.
	Live bool
	// Status is hc's OSC 7501 state: idle, working, done, blocked, error,
	// or "" when the program has reported none yet.
	Status string
	// App is the program that reported the status, such as "claude-code".
	App string
	// Turn is hc's turn counter for the terminal.
	Turn int
	// Text is the screen plus up to screenLines rows of history. It is
	// untrusted: matched against, never rendered raw.
	Text string
}

// WorkerStatus reads the worker's status without its screen.
func (a *Adapter) WorkerStatus(ctx context.Context, ref backend.Ref) (Worker, error) {
	t, err := a.owned(ctx, ref)
	if err != nil {
		return Worker{}, err
	}
	return Worker{Live: t.State == stateLive, Status: t.Status.State, App: t.Status.App, Turn: t.Turn}, nil
}

// WorkerScreen reads the worker's status and its screen text.
func (a *Adapter) WorkerScreen(ctx context.Context, ref backend.Ref) (Worker, error) {
	t, err := a.owned(ctx, ref)
	if err != nil {
		return Worker{}, err
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHcRead,
		exec.MustFixed("read"),
		exec.MustFixed("--json"),
		exec.MustFixed("--lines"),
		exec.MustFixed(screenLines),
		exec.EndOfOptions(),
		exec.Opaque(t.Address),
	))
	if runErr != nil {
		return Worker{}, fmt.Errorf("%w: hc read: %v", ErrUnreadable, classifyRunError(runErr))
	}
	var read struct {
		Complete bool   `json:"complete"`
		Text     string `json:"text"`
	}
	if derr := decodeStdout(res.Stdout, &read); derr != nil {
		return Worker{}, fmt.Errorf("%w: %v", ErrUnreadable, derr)
	}
	// hc says when it could not return the whole screen and history. A
	// partial read could hide a REPORT line and read as "no report yet".
	if !read.Complete {
		return Worker{}, fmt.Errorf("%w: hc read returned an incomplete screen", ErrUnreadable)
	}
	return Worker{
		Live:   t.State == stateLive,
		Status: t.Status.State,
		App:    t.Status.App,
		Turn:   t.Turn,
		Text:   strings.TrimRight(read.Text, "\n"),
	}, nil
}

// SendResult is what hc reported for an accepted send.
type SendResult struct {
	// Status is the worker's status hc reported with the send, or "".
	Status string
	// Turn is hc's turn counter after the send.
	Turn int
}

// SendBrief pastes text into the worker and submits it, with `hc send`.
//
// It never passes --force: hc refuses a blocked terminal, and that refusal is
// the operator's decision to make, not forgectl's. checked is the status and
// turn the caller read before its own checks and the Escape. SendBrief
// re-reads the terminal and refuses, typing nothing, unless it is still
// forgectl's, live, at the checked turn, and idle or done: a turn that started
// since the check, or a dialog hc now reports as blocked, gets no text.
//
// When either the checked or the re-read status is idle, hc also types only
// while it is still idle (`--if idle`), which closes the window to the one hc
// call. hc's --if takes no "done", so a worker checked and still done is sent
// to without it, and the re-read is the
// whole check: the window left is one hc round trip, the same as PressEscape's.
// The re-read accepts idle where done was checked (and the reverse): the Escape
// before the send may make a harness report again, and neither status means a
// turn is running.
//
// Every failure before hc is asked, the identity checks included, wraps
// ErrSendRefused: nothing was typed.
func (a *Adapter) SendBrief(ctx context.Context, ref backend.Ref, text string, checked Worker) (SendResult, error) {
	t, err := a.owned(ctx, ref)
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: %w", ErrSendRefused, err)
	}
	if t.State != stateLive {
		return SendResult{}, fmt.Errorf("%w: the worker's program has exited", ErrSendRefused)
	}
	if !atPrompt(t.Status.State) || t.Turn != checked.Turn {
		return SendResult{}, fmt.Errorf("%w: the worker changed since it was checked (status %q turn %d, now %q turn %d)",
			ErrSendRefused, checked.Status, checked.Turn, t.Status.State, t.Turn)
	}
	args := []exec.Arg{exec.MustFixed("send"), exec.MustFixed("--json")}
	// --if idle goes with the send when either read saw idle: a worker
	// checked idle and re-read done keeps hc's own guard, which then refuses
	// rather than typing into a worker whose status moved.
	if t.Status.State == StatusIdle || checked.Status == StatusIdle {
		args = append(args, exec.MustFixed("--if"), exec.MustFixed("idle"))
	}
	args = append(args, exec.EndOfOptions(), exec.Opaque(t.Address), exec.Opaque(text))
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHcSend, args...))
	if runErr != nil {
		switch kind := errorKind(res.Stderr); kind {
		case kindBlocked, kindStatusMismatch, kindFinished:
			return SendResult{}, fmt.Errorf("%w: hc reports %s", ErrSendRefused, refusalKind(kind))
		}
		return SendResult{}, fmt.Errorf("%w: %v", ErrSendFailed, classifyRunError(runErr))
	}
	var sent struct {
		Status status `json:"status"`
		Turn   int    `json:"turn"`
	}
	if derr := decodeStdout(res.Stdout, &sent); derr != nil {
		// hc accepted the send; only its reply is unreadable.
		return SendResult{}, nil
	}
	return SendResult{Status: sent.Status.State, Turn: sent.Turn}, nil
}

// atPrompt reports a status at which a worker waits for input: idle or done.
func atPrompt(status string) bool { return status == StatusIdle || status == StatusDone }

// PressEscape sends one Escape to the worker with `hc keys`, so the first key
// any open dialog sees is a refusal: Claude Code's Esc closes a dialog and
// declines a permission prompt, Codex maps Esc to cancel on its approval
// prompts, and pi's Escape cancels a selection. These are each harness's
// default bindings; an operator who rebinds Escape voids this. At an idle
// prompt one Escape does nothing a brief cares about; only a second Escape
// within the harness's double-press window opens a menu.
//
// Escape also interrupts a running turn, and hc keys has no idle guard like
// send's --if idle. So it first re-reads the terminal and refuses unless it
// is still forgectl's, live, and at the status and turn the caller checked
// (want): a turn that started since then is left alone. The window left is
// the one hc round trip between that read and the key.
func (a *Adapter) PressEscape(ctx context.Context, ref backend.Ref, want Worker) error {
	t, err := a.owned(ctx, ref)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSendRefused, err)
	}
	if t.State != stateLive {
		return fmt.Errorf("%w: the worker's program has exited", ErrSendRefused)
	}
	if t.Status.State != want.Status || t.Turn != want.Turn {
		return fmt.Errorf("%w: the worker changed since it was checked (status %q turn %d, now %q turn %d)",
			ErrSendRefused, want.Status, want.Turn, t.Status.State, t.Turn)
	}
	if _, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHcSend,
		exec.MustFixed("keys"),
		exec.MustFixed("--json"),
		exec.EndOfOptions(),
		exec.Opaque(t.Address),
		exec.MustFixed("esc"),
	)); runErr != nil {
		return fmt.Errorf("%w: hc keys: %v", ErrSendFailed, classifyRunError(runErr))
	}
	return nil
}

func refusalKind(kind string) string {
	switch kind {
	case kindBlocked:
		return "the worker is blocked"
	case kindFinished:
		return "the worker has finished"
	default:
		return "a status mismatch"
	}
}

// TerminalID returns the worker's hc address, the handle `hc send` and
// `hc read` take.
func (a *Adapter) TerminalID(ctx context.Context, ref backend.Ref) (string, error) {
	t, err := a.owned(ctx, ref)
	if err != nil {
		return "", err
	}
	return t.Address, nil
}

// owned locates the reference's terminal and maps the outcome onto the
// worker errors.
func (a *Adapter) owned(ctx context.Context, ref backend.Ref) (terminal, error) {
	t, state, cause := a.locate(ctx, ref)
	switch state {
	case locateFound:
		return t, nil
	case locateAbsent:
		return terminal{}, ErrWorkerGone
	case locateMismatch, locateUnreadable, locateInvalid:
	}
	return terminal{}, fmt.Errorf("%w: %v", ErrUnreadable, cause)
}

// SendLine types text into the hc terminal at addr and submits it, with `hc
// send`. Unlike SendBrief it works on any terminal, forgectl's or not: the
// desk uses it to tell an asking agent that the operator answered. It never
// passes --force, so hc's own refusal of a blocked terminal stands.
func (a *Adapter) SendLine(ctx context.Context, addr, text string) error {
	if addr == "" || strings.ContainsAny(addr, " \t\r\n") || strings.HasPrefix(addr, "-") {
		return fmt.Errorf("%w: %q is not a terminal address", ErrSendRefused, addr)
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: the text must be one line with no control or format characters", ErrSendRefused)
		}
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHcSend,
		exec.MustFixed("send"),
		exec.MustFixed("--json"),
		exec.EndOfOptions(),
		exec.Opaque(addr),
		exec.Opaque(text),
	))
	if runErr != nil {
		switch kind := errorKind(res.Stderr); kind {
		case kindBlocked, kindStatusMismatch, kindFinished:
			return fmt.Errorf("%w: hc reports %s", ErrSendRefused, refusalKind(kind))
		}
		return fmt.Errorf("%w: %v", ErrSendFailed, classifyRunError(runErr))
	}
	return nil
}
