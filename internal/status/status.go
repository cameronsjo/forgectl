// Package status collects the sections of `forgectl status`, the read-only
// cross-project overview (forgectl#13). It owns no data source of its own:
// each section is a function the CLI layer wires to a read path that already
// ships (projects discovery, the `pr dash` sections, the clean dry-run scan,
// the bench health card). What this package adds is the containment rule:
// every section runs under its own deadline, and a section that errors, runs
// out of time, or panics on the source's own goroutine degrades to a
// per-section failure instead of failing the command. (A goroutine the source
// starts itself is outside that recover; its panic still ends the process.)
package status

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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

// errPanicked is the categorical text for a source that panicked on its own
// goroutine. The panic value itself is never rendered or logged: it can be
// any value from any depth. Only its Go type reaches the debug log.
var errPanicked = errors.New("source panicked")

// outcome carries a source's return values across the goroutine boundary.
// ctxErr is the section context's state the moment the source returned,
// captured on the source's own goroutine: whether the result beat the
// deadline is a fact about when the source finished, not about when Collect
// got round to reading the channel.
type outcome[T any] struct {
	data   T
	notes  []string
	err    error
	ctxErr error
}

// Collect runs src under a deadline of timeout and folds its outcome into a
// Section. It never returns an error, and a panic on the source's own
// goroutine becomes a failed section rather than a crash.
//
// A section whose context had ended by the time its source returned is failed
// with the deadline, whatever the source returned. The shipped sources turn
// cancellation into ordinary-looking data (an "unknown" tree, a target
// skipped as dirty, "docker compose unavailable"), so a result produced after
// the deadline cannot be told apart from a real answer and is never reported
// as one. A source that returned before the deadline is judged on what it
// returned, even when Collect reads it after the deadline has passed.
//
// The source runs on its own goroutine so a source that ignores its context
// (the clean walk takes none) is abandoned at the deadline instead of holding
// the whole report. The abandoned goroutine keeps running until it returns;
// its result lands in a buffered channel nobody reads, so it never blocks.
// A caller that runs Collect repeatedly (the cockpit) uses CollectTracked to
// learn when that goroutine has actually returned.
//
// timeout < 1 means no deadline beyond ctx's own.
func Collect[T any](ctx context.Context, timeout time.Duration, src Source[T]) Section[T] {
	s, _ := CollectTracked(ctx, timeout, src)
	return s
}

// CollectTracked is Collect, plus a channel that is closed once the source's
// goroutine has returned. Collect gives up on a source at its deadline, but
// the goroutine runs on until the source returns; a long-running caller that
// starts the same source again before then would pile up abandoned
// goroutines. Such a caller treats the section as busy until done is closed.
func CollectTracked[T any](ctx context.Context, timeout time.Duration, src Source[T]) (Section[T], <-chan struct{}) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	r := &result[T]{ch: make(chan outcome[T], 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				slog.Debug("Status section source panicked.", "type", fmt.Sprintf("%T", p))
				r.send(ctx, outcome[T]{err: errPanicked})
			}
		}()
		data, notes, err := src(ctx)
		r.send(ctx, outcome[T]{data: data, notes: notes, err: err})
	}()

	select {
	case o := <-r.ch:
		return fold(o, timeout), done
	case <-ctx.Done():
		return r.settle(ctx.Err(), timeout), done
	}
}

// result is the one-slot handoff between a source's goroutine and Collect.
// mu makes "read the context, then send" one step as far as settle can see,
// which is what closes the race at the deadline: if settle finds the slot
// empty, the source had not yet read its context, and when it does it will
// find the context ended.
type result[T any] struct {
	mu sync.Mutex
	ch chan outcome[T]
}

// send stamps o with the context's state at the moment the source returned
// and hands it over. ch has room for the one send, so it never blocks.
func (r *result[T]) send(ctx context.Context, o outcome[T]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o.ctxErr = ctx.Err()
	r.ch <- o
}

// settle decides a section whose context ended while Collect was waiting.
// When both the result and the deadline are ready, select picks between them
// at random, so a source that returned just before its deadline could lose
// the draw; settle takes a result already handed over first, and fold judges
// it on the context state captured when the source returned.
func (r *result[T]) settle(ctxErr error, timeout time.Duration) Section[T] {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case o := <-r.ch:
		return fold(o, timeout)
	default:
		return failed[T](deadlineError(ctxErr, timeout))
	}
}

// fold turns a returned outcome into a Section. Once o.ctxErr is set, the
// section failed on its deadline whatever the source returned, error or
// data: data read under a cancelled context is not an answer (see Collect).
func fold[T any](o outcome[T], timeout time.Duration) Section[T] {
	if o.ctxErr != nil {
		return failed[T](deadlineError(o.ctxErr, timeout))
	}
	if o.err != nil {
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
