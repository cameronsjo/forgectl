package cli

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
)

// docsServePublishAttempts bounds how many times startup will try to install a
// discovery record. Only ErrGenerationCollision consumes an attempt, and a
// collision means two 128-bit random names landed on the same value — so eight
// is not a tuning knob, it is a ceiling on a loop that should never take a
// second pass.
const docsServePublishAttempts = 8

// Fixed startup failures. They carry no address, generation, token, or path:
// the discovery directory holds names any process running as this user can
// choose, and a startup error is the wrong place to find out what they are.
var (
	errDocsServeGeneration = errors.New("could not mint a docs discovery generation")
	errDocsServeProbe      = errors.New("the docs server did not answer its own startup discovery probe")
	errDocsServeInternal   = errors.New("docs discovery publication reported success without a lease")
)

// docsServeDiscoveryUnavailable is printed once when the server will keep
// serving but will not be discoverable.
//
// Continuing to serve is deliberate. An unprotected loopback server is still
// usable through the URL it printed, and killing a healthy reader because a
// bookkeeping file could not be written would be a worse trade than a warning.
const docsServeDiscoveryUnavailable = "warning: `forgectl docs open` will not find this server — it is serving, but its discovery record could not be published"

// docsServeEventKind names the three things startup and steady-state serving
// can be interrupted by.
type docsServeEventKind uint8

const (
	docsServeEventServe docsServeEventKind = iota + 1
	docsServeEventCancel
	docsServeEventOperation
)

type docsServeEvent struct {
	Kind docsServeEventKind
	Err  error
}

// pollDocsServePrimary is the nonblocking checkpoint: is a higher-priority
// outcome ALREADY decided?
//
// Priority is total and fixed — Serve result, then cancellation, then whatever
// discovery operation is in flight — and it is checked with ordered nonblocking
// receives rather than a select over all three. A Go select among ready cases
// picks at random, so using one here would mean a server that had already
// failed to serve could still publish a discovery record, depending on a coin
// flip. That is the bug this function exists to make impossible.
func pollDocsServePrimary(ctx context.Context, serve <-chan error) (docsServeEvent, bool) {
	select {
	case err := <-serve:
		return docsServeEvent{Kind: docsServeEventServe, Err: err}, true
	default:
	}
	select {
	case <-ctx.Done():
		return docsServeEvent{Kind: docsServeEventCancel}, true
	default:
	}
	return docsServeEvent{}, false
}

// awaitDocsServeEvent blocks until one of the three sources produces a result,
// then applies the same total priority before returning.
//
// The blocking select can legitimately wake on the lowest-priority source while
// a higher one was also ready. So after consuming an operation result it
// re-runs the ordered checkpoint and lets a higher event win — the operation
// result is then deliberately dropped, because every caller that receives a
// higher event is on its way out and will never act on it.
//
// operation may be nil, which is the steady-state Serve-versus-cancellation
// wait: a receive from a nil channel blocks forever, so that case simply never
// fires.
func awaitDocsServeEvent(ctx context.Context, serve <-chan error, operation <-chan error) docsServeEvent {
	if event, ok := pollDocsServePrimary(ctx, serve); ok {
		return event
	}

	var consumed docsServeEvent
	select {
	case err := <-serve:
		return docsServeEvent{Kind: docsServeEventServe, Err: err}
	case <-ctx.Done():
		// Cancellation and a Serve result can become ready together; Serve
		// outranks it.
		if event, ok := pollDocsServePrimary(ctx, serve); ok {
			return event
		}
		return docsServeEvent{Kind: docsServeEventCancel}
	case err := <-operation:
		consumed = docsServeEvent{Kind: docsServeEventOperation, Err: err}
	}

	if event, ok := pollDocsServePrimary(ctx, serve); ok {
		return event
	}
	return consumed
}

// publishOutcome is what the discovery startup sequence produces.
//
// primary is non-nil when a Serve result or a cancellation won an arbitration
// mid-sequence: the caller must exit under that event rather than continuing to
// start up. lease is non-nil whenever a record was installed, INCLUDING when a
// higher event won during the publish call — the record exists either way and
// something has to close it.
type publishOutcome struct {
	lease   *docspkg.ServerLease
	primary *docsServeEvent
}

// docsDiscoverySession carries the state the publication sequence needs, so the
// sequence itself reads as the state machine it is rather than as a function
// with nine parameters.
type docsDiscoverySession struct {
	rt         docsServeRuntime
	dir        string
	token      string
	generation *atomic.Value
	serve      <-chan error
	background *sync.WaitGroup
	errOut     io.Writer
}

// publish self-probes and installs the discovery record.
//
// The ordering rule it enforces: a record naming this server becomes visible
// only AFTER this server has answered a request for the exact generation that
// record names. Publishing first and serving second — which is what the code
// this replaces did — advertises an address before anything is listening on it,
// so a reader can find a server that cannot yet answer.
//
// A non-nil error is a startup failure. A nil error with a nil lease means
// discovery is unavailable and serving continues; the warning has already been
// printed.
func (s *docsDiscoverySession) publish(ctx context.Context, initial docspkg.ServerInfo) (publishOutcome, error) {
	current := initial

	for attempt := 1; attempt <= docsServePublishAttempts; attempt++ {
		event, err := s.selfProbe(ctx, current)
		if err != nil {
			return publishOutcome{}, err
		}
		if event != nil {
			return publishOutcome{primary: event}, nil
		}

		// Re-check between a successful probe and the publish call: the server
		// may have failed or been signalled while the probe was in flight, and
		// a record installed after that would outlive the server it names by
		// however long shutdown takes.
		if higher, ok := pollDocsServePrimary(ctx, s.serve); ok {
			return publishOutcome{primary: &higher}, nil
		}

		publication, err := s.rt.publish(s.dir, current)
		higher, hasHigher := pollDocsServePrimary(ctx, s.serve)

		if err == nil {
			if publication.Warning != nil {
				warnDocsServe(s.errOut, "warning: the docs discovery record may not be durable: %v", publication.Warning)
			}
			if publication.Lease == nil {
				if hasHigher {
					return publishOutcome{primary: &higher}, nil
				}
				return publishOutcome{}, errDocsServeInternal
			}
			if hasHigher {
				return publishOutcome{lease: publication.Lease, primary: &higher}, nil
			}
			return publishOutcome{lease: publication.Lease}, nil
		}

		if hasHigher {
			// The server is already exiting. Retrying or warning about
			// discovery now would be noise about a server nobody will use.
			return publishOutcome{primary: &higher}, nil
		}
		if !errors.Is(err, docspkg.ErrGenerationCollision) {
			warnDocsServe(s.errOut, "%s", docsServeDiscoveryUnavailable)
			return publishOutcome{}, nil
		}
		if attempt == docsServePublishAttempts {
			warnDocsServe(s.errOut, "%s", docsServeDiscoveryUnavailable)
			return publishOutcome{}, nil
		}

		next, event, err := s.retryGeneration(ctx, current)
		if err != nil {
			return publishOutcome{}, err
		}
		if event != nil {
			return publishOutcome{primary: event}, nil
		}
		if next == nil {
			return publishOutcome{}, nil
		}
		current = *next
	}
	return publishOutcome{}, nil
}

// selfProbe asks this server, over the network, whether it is serving the
// generation it is about to publish.
//
// Scheduling the Serve goroutine is not a readiness barrier — the listener may
// not be accepting yet — so the probe is the barrier. It runs against a child
// context cancelled on every exit from the attempt, so no probe outlives the
// attempt that started it.
//
// A nil event with a nil error means the probe succeeded. A non-nil event means
// a higher-priority outcome won and startup must exit under it.
func (s *docsDiscoverySession) selfProbe(ctx context.Context, info docspkg.ServerInfo) (*docsServeEvent, error) {
	probeCh := make(chan error, 1)
	probeCtx, cancelProbe := context.WithCancel(ctx)
	defer cancelProbe()

	s.background.Add(1)
	go func(addr, generation string) {
		defer s.background.Done()
		probeCh <- s.rt.probe(probeCtx, addr, generation)
	}(info.Addr, info.Generation)

	event := awaitDocsServeEvent(ctx, s.serve, probeCh)
	if event.Kind != docsServeEventOperation {
		return &event, nil
	}
	if event.Err != nil {
		// A discovery-eligible server that cannot answer for its own
		// generation is not serving correctly. This is a startup failure, not
		// a discovery warning.
		return nil, errDocsServeProbe
	}
	return nil, nil
}

// retryGeneration mints a replacement generation after a collision.
//
// The two checkpoints around the mint are not redundant. The first refuses to
// start work the server is already exiting from; the second refuses to ACT on a
// result that arrived after a higher event became ready. Between them, a
// cancelled server never updates its identity endpoint or publishes again.
//
// A nil next with a nil event and a nil error means the mint failed: discovery
// is unavailable, the warning is printed, and serving continues. The failure is
// deliberately not fatal — discovery already degrades safely on collision
// exhaustion, and a healthy reader is worth more than a consistent failure mode.
func (s *docsDiscoverySession) retryGeneration(ctx context.Context, current docspkg.ServerInfo) (*docspkg.ServerInfo, *docsServeEvent, error) {
	if higher, ok := pollDocsServePrimary(ctx, s.serve); ok {
		return nil, &higher, nil
	}

	next, err := s.rt.newInfo(current.Addr, s.token)

	if higher, ok := pollDocsServePrimary(ctx, s.serve); ok {
		return nil, &higher, nil
	}
	if err != nil {
		warnDocsServe(s.errOut, "%s", docsServeDiscoveryUnavailable)
		return nil, nil, nil
	}

	// Store BEFORE the next probe, so the identity endpoint answers for the
	// generation that probe is about to ask for. Storing after would make the
	// probe a test of the previous generation.
	s.generation.Store(next.Generation)
	return &next, nil, nil
}
