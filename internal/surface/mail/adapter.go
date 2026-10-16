package mail

import (
	"context"
	"errors"
	"fmt"
)

// Adapter hands a rendered message to one harness and reports its state.
type Adapter interface {
	// Deliver hands text to the worker's harness. It returns a short detail
	// for the mailbox, or an error. Wrap a transient error with NotReady so the
	// message stays queued; any other error fails it.
	Deliver(ctx context.Context, w Worker, m Message, text string) (string, error)
	// State reports the worker's activity as the harness sees it.
	State(ctx context.Context, w Worker) (WorkerState, error)
}

// ReceiptAdapter reconciles a delivery whose transport outcome was uncertain.
// It must not deliver the message again. An accepted receipt proves only that
// the harness accepted the message, not that the agent read it or completed a
// turn.
type ReceiptAdapter interface {
	Receipt(ctx context.Context, worker Worker, message Message) (detail string, accepted bool, err error)
}

type retryable struct{ err error }

func (r retryable) Error() string { return r.err.Error() }
func (r retryable) Unwrap() error { return r.err }

// NotReady marks a delivery error as transient: the recipient has not started,
// its socket refused the connection, or it is not at its prompt.
func NotReady(format string, a ...any) error {
	return retryable{fmt.Errorf(format, a...)}
}

// IsRetryable reports whether err came from NotReady.
func IsRetryable(err error) bool {
	var r retryable
	return errors.As(err, &r)
}

type unconfirmed struct{ err error }

func (uncertain unconfirmed) Error() string { return uncertain.err.Error() }
func (uncertain unconfirmed) Unwrap() error { return uncertain.err }

// Unconfirmed marks a delivery whose external outcome cannot be established.
// The mailbox must not automatically deliver it again.
func Unconfirmed(format string, args ...any) error {
	return unconfirmed{fmt.Errorf(format, args...)}
}

// IsUnconfirmed reports whether err carries an uncertain external outcome.
func IsUnconfirmed(err error) bool {
	var uncertain unconfirmed
	return errors.As(err, &uncertain)
}
