package cli

// Test plan for the docs-serve arbitration helpers.
//
// These tables PRELOAD every channel and the context before calling the helper,
// so the assertion is about the priority rule and not about which goroutine the
// scheduler happened to run. That distinction is the whole point: the bug these
// helpers exist to prevent is a raw `select` over several ready cases, which Go
// resolves at RANDOM — a test that raced real goroutines would pass most of the
// time against exactly the code being ruled out.
//
// pollDocsServePrimary (Classification: ordering gate — nonblocking checkpoint)
//   [x] nothing ready reports no event
//   [x] Serve alone, cancel alone
//   [x] Serve + cancel selects Serve
//   [x] a winning Serve result is consumed, not left buffered
//
// awaitDocsServeEvent (Classification: ordering gate — blocking arbitration)
//   [x] operation alone selects the operation
//   [x] Serve + operation selects Serve
//   [x] cancel + operation selects cancellation
//   [x] Serve + cancel selects Serve
//   [x] all three select Serve
//   [x] a nil operation channel is the steady-state Serve/cancel wait

import (
	"context"
	"errors"
	"testing"
	"time"
)

// arbitrationRow describes one fully preloaded arbitration state.
type arbitrationRow struct {
	name        string
	serveReady  bool
	serveErr    error
	cancelled   bool
	opReady     bool
	opErr       error
	nilOp       bool
	wantKind    docsServeEventKind
	wantErr     error
	wantDecided bool // pollDocsServePrimary only
}

// preload builds the exact state a row describes. Every channel is buffered and
// filled before the helper is called, so no goroutine has to run for the helper
// to observe the state.
func (r arbitrationRow) preload(t *testing.T) (context.Context, chan error, chan error) {
	t.Helper()
	serve := make(chan error, 1)
	if r.serveReady {
		serve <- r.serveErr
	}
	var operation chan error
	if !r.nilOp {
		operation = make(chan error, 1)
		if r.opReady {
			operation <- r.opErr
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if r.cancelled {
		cancel()
	}
	return ctx, serve, operation
}

var errServeFailed = errors.New("serve failed")
var errProbeFailed = errors.New("probe failed")

func TestPollDocsServePrimary_Priority(t *testing.T) {
	rows := []arbitrationRow{
		{name: "nothing ready", wantDecided: false},
		{name: "serve alone", serveReady: true, serveErr: errServeFailed, wantDecided: true, wantKind: docsServeEventServe, wantErr: errServeFailed},
		{name: "cancel alone", cancelled: true, wantDecided: true, wantKind: docsServeEventCancel},
		{
			name: "serve and cancel together", serveReady: true, serveErr: errServeFailed, cancelled: true,
			wantDecided: true, wantKind: docsServeEventServe, wantErr: errServeFailed,
		},
		{
			name: "serve nil error and cancel together", serveReady: true, cancelled: true,
			wantDecided: true, wantKind: docsServeEventServe,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// Repeat: a random `select` would pass a single run roughly half
			// the time on the contended rows.
			for i := 0; i < 200; i++ {
				ctx, serve, _ := row.preload(t)
				event, decided := pollDocsServePrimary(ctx, serve)
				if decided != row.wantDecided {
					t.Fatalf("run %d: decided = %v, want %v", i, decided, row.wantDecided)
				}
				if !decided {
					continue
				}
				if event.Kind != row.wantKind {
					t.Fatalf("run %d: kind = %d, want %d", i, event.Kind, row.wantKind)
				}
				if !errors.Is(event.Err, row.wantErr) {
					t.Fatalf("run %d: err = %v, want %v", i, event.Err, row.wantErr)
				}
			}
		})
	}
}

func TestPollDocsServePrimary_ServeResultIsConsumedNotPeeked(t *testing.T) {
	// The Serve result must come OUT of the channel when it wins, because the
	// caller returns it. Leaving it buffered would mean a later checkpoint saw
	// the same event again and acted on it twice.
	serve := make(chan error, 1)
	serve <- errServeFailed

	if _, decided := pollDocsServePrimary(context.Background(), serve); !decided {
		t.Fatal("the buffered Serve result was not observed")
	}
	if len(serve) != 0 {
		t.Fatalf("the Serve channel still holds %d results after the checkpoint consumed one", len(serve))
	}
}

func TestAwaitDocsServeEvent_Priority(t *testing.T) {
	rows := []arbitrationRow{
		{
			name: "operation alone", opReady: true, opErr: errProbeFailed,
			wantKind: docsServeEventOperation, wantErr: errProbeFailed,
		},
		{
			name: "operation success alone", opReady: true,
			wantKind: docsServeEventOperation,
		},
		{
			name: "serve outranks operation", serveReady: true, serveErr: errServeFailed, opReady: true,
			wantKind: docsServeEventServe, wantErr: errServeFailed,
		},
		{
			name: "cancel outranks operation", cancelled: true, opReady: true,
			wantKind: docsServeEventCancel,
		},
		{
			name: "serve outranks cancel", serveReady: true, serveErr: errServeFailed, cancelled: true,
			wantKind: docsServeEventServe, wantErr: errServeFailed,
		},
		{
			name: "serve outranks cancel and operation", serveReady: true, serveErr: errServeFailed, cancelled: true, opReady: true,
			wantKind: docsServeEventServe, wantErr: errServeFailed,
		},
		{
			name: "steady state serve with nil operation", nilOp: true, serveReady: true, serveErr: errServeFailed,
			wantKind: docsServeEventServe, wantErr: errServeFailed,
		},
		{
			name: "steady state cancel with nil operation", nilOp: true, cancelled: true,
			wantKind: docsServeEventCancel,
		},
		{
			name: "steady state serve outranks cancel", nilOp: true, serveReady: true, cancelled: true,
			wantKind: docsServeEventServe,
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for i := 0; i < 200; i++ {
				ctx, serve, operation := row.preload(t)

				result := make(chan docsServeEvent, 1)
				go func() { result <- awaitDocsServeEvent(ctx, serve, operation) }()

				select {
				case event := <-result:
					if event.Kind != row.wantKind {
						t.Fatalf("run %d: kind = %d, want %d", i, event.Kind, row.wantKind)
					}
					if !errors.Is(event.Err, row.wantErr) {
						t.Fatalf("run %d: err = %v, want %v", i, event.Err, row.wantErr)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("run %d: awaitDocsServeEvent blocked on a fully preloaded state", i)
				}
			}
		})
	}
}

func TestAwaitDocsServeEvent_NilOperationBlocksUntilAHigherEvent(t *testing.T) {
	serve := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan docsServeEvent, 1)
	go func() { result <- awaitDocsServeEvent(ctx, serve, nil) }()

	select {
	case event := <-result:
		t.Fatalf("awaitDocsServeEvent returned %+v with nothing ready — a nil operation channel must never fire", event)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case event := <-result:
		if event.Kind != docsServeEventCancel {
			t.Fatalf("kind = %d, want cancellation", event.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitDocsServeEvent did not wake on cancellation")
	}
}
