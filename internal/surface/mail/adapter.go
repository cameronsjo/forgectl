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
