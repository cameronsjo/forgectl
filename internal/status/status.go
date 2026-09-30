// Package status collects the sections of `forgectl status`, the read-only
// cross-project overview (forgectl#13). It owns no data source of its own:
// each section is a function the CLI layer wires to a read path that already
// ships (projects discovery, the `pr dash` sections, the clean dry-run scan,
// the bench health card). What this package adds is the containment rule:
// every section runs under its own deadline, and a section that errors,
// panics, or runs out of time degrades to a per-section failure instead of
// failing the command.
package status

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// State is one section's collection outcome. It says whether the section's
// source could be read, not whether what it read is healthy: a bench
// component that is down is an "ok" section whose data says "unavailable".
type State string

const (
	// StateOK means the source answered in full.
	StateOK State = "ok"
	// StateDegraded means the source answered, but some part of it could not
	// be read; the notes name which part. Data is present.
	StateDegraded State = "degraded"
	// StateFailed means the source produced no data: it returned an error,
	// panicked, or missed its deadline. Error says which; Data is null.
	StateFailed State = "failed"
)

// ErrorMaxRunes caps a section's error text. A source error can carry a
// filesystem path or subprocess text nobody at the terminal chose, so its
// length is bounded on the machine path too, not just in the human view.
const ErrorMaxRunes = 200

// NoteMaxRunes caps each degradation note for the same reason.
const NoteMaxRunes = 200

// Section is one source's slot in the report. Every field is always present
// on the wire: Error is "" unless the section failed, Notes is [] rather than
// null, and Data is null only when State is "failed".
type Section[T any] struct {
	State State    `json:"state"`
	Error string   `json:"error"`
	Notes []string `json:"notes"`
	Data  *T       `json:"data"`
}

// Source reads one section. It returns the section's data, any degradation
// notes (each one a short categorical line), and an error when it could
// produce no data at all.
type Source[T any] func(ctx context.Context) (T, []string, error)

// errPanicked is the categorical text for a source that panicked. The panic
// value itself is never rendered: it can be any value from any depth.
var errPanicked = errors.New("source panicked")

// outcome carries a source's return values across the goroutine boundary.
type outcome[T any] struct {
	data  T
	notes []string
	err   error
}

// Collect runs src under a deadline of timeout and folds its outcome into a
// Section. It never returns an error and never panics on the source's behalf.
//
// The source runs on its own goroutine so a source that ignores its context
// (the clean walk takes none) is abandoned at the deadline instead of holding
// the whole report. The abandoned goroutine keeps running until it returns,
// which for a one-shot CLI process means at most until exit; its result lands
// in a buffered channel nobody reads, so it never blocks.
//
// timeout < 1 means no deadline beyond ctx's own.
func Collect[T any](ctx context.Context, timeout time.Duration, src Source[T]) Section[T] {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ch := make(chan outcome[T], 1)
	go func() {
		defer func() {
			if recover() != nil {
				ch <- outcome[T]{err: errPanicked}
			}
		}()
		data, notes, err := src(ctx)
		ch <- outcome[T]{data: data, notes: notes, err: err}
	}()

	select {
	case o := <-ch:
		return fold(o, ctx.Err(), timeout)
	case <-ctx.Done():
		// select picks at random when both are ready; a result that did
		// arrive wins over the deadline it raced.
		select {
		case o := <-ch:
			return fold(o, ctx.Err(), timeout)
		default:
		}
		return failed[T](deadlineError(ctx.Err(), timeout))
	}
}

// fold turns a returned outcome into a Section. ctxErr is the section
// context's state at return: a source that gave up because its deadline
// passed reports the deadline, which says more than the subprocess error the
// cancellation produced.
func fold[T any](o outcome[T], ctxErr error, timeout time.Duration) Section[T] {
	if o.err != nil {
		if ctxErr != nil {
			return failed[T](deadlineError(ctxErr, timeout))
		}
		return failed[T](o.err)
	}
	notes := make([]string, 0, len(o.notes))
	for _, n := range o.notes {
		notes = append(notes, termsafe.SafeLineMax(n, NoteMaxRunes))
	}
	state := StateOK
	if len(notes) > 0 {
		state = StateDegraded
	}
	data := o.data
	return Section[T]{State: state, Error: "", Notes: notes, Data: &data}
}

// failed builds a failed Section whose error text is escaped and capped.
func failed[T any](err error) Section[T] {
	return Section[T]{
		State: StateFailed,
		Error: termsafe.SafeLineMax(termsafe.Error(err).Error(), ErrorMaxRunes),
		Notes: []string{},
	}
}

// deadlineError names why a section's context ended.
func deadlineError(ctxErr error, timeout time.Duration) error {
	if errors.Is(ctxErr, context.DeadlineExceeded) && timeout > 0 {
		return fmt.Errorf("timed out after %s", timeout)
	}
	if ctxErr == nil {
		return errors.New("stopped")
	}
	return ctxErr
}
