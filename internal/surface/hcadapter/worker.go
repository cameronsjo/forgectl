package hcadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// the operator's decision to make, not forgectl's. The caller checks the
// status first; this re-checks that the terminal is forgectl's and live.
func (a *Adapter) SendBrief(ctx context.Context, ref backend.Ref, text string) (SendResult, error) {
	t, err := a.owned(ctx, ref)
	if err != nil {
		return SendResult{}, err
	}
	if t.State != stateLive {
		return SendResult{}, fmt.Errorf("%w: the worker's program has exited", ErrSendRefused)
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHcSend,
		exec.MustFixed("send"),
		exec.MustFixed("--json"),
		exec.EndOfOptions(),
		exec.Opaque(t.Address),
		exec.Opaque(text),
	))
	if runErr != nil {
		switch res.ExitCode {
		case exitBlocked, exitStatusMismatch, exitFinished:
			return SendResult{}, fmt.Errorf("%w: hc reports %s", ErrSendRefused, refusalKind(res.ExitCode))
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

func refusalKind(code int) string {
	switch code {
	case exitBlocked:
		return "the worker is blocked"
	case exitFinished:
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
