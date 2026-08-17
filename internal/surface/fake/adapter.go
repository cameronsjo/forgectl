// Package fake provides a scriptable surface adapter for tests.
//
// It exists in its own package rather than as a test helper inside
// internal/surface for one structural reason: it is the subject of the import
// barrier. An adapter must not be able to see a launch invocation, and the way
// that is proven is by asserting this package's transitive dependencies do not
// include internal/launch. A helper living inside internal/surface — which
// legitimately imports launch for the binary-provenance policy — could not
// carry that proof, and would in fact break it for every real adapter.
//
// The recorded specs are kept as whole values, so a test asserts against the
// opaque bootstrap through exec.Arg.Equal rather than through a reveal
// accessor that production has no equivalent of.
package fake

import (
	"context"
	"sync"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Adapter is a backend.Capabilities implementation driven by scripted
// responses. The zero value is not usable: New requires a backend kind,
// because an adapter that reports no kind is one the capability preflight
// refuses.
type Adapter struct {
	kind backend.Kind

	mu     sync.Mutex
	starts []backend.StartSpec
	closes []backend.Ref
	probes []backend.Ref

	// StartFunc, CloseFunc, and ProbeFunc script each call. A nil function
	// means the corresponding call was not expected: it returns an unclassified
	// zero result, which fails Validate and surfaces as a test failure at the
	// point the service checks it rather than as a silent success.
	StartFunc func(ctx context.Context, spec backend.StartSpec) backend.StartResult
	CloseFunc func(ctx context.Context, ref backend.Ref) backend.CloseResult
	ProbeFunc func(ctx context.Context, ref backend.Ref) backend.ProbeResult
}

// New builds a fake adapter for one backend.
func New(kind backend.Kind) *Adapter { return &Adapter{kind: kind} }

// Kind reports the backend this fake stands in for.
func (a *Adapter) Kind() backend.Kind { return a.kind }

// Start records the spec and returns the scripted result.
func (a *Adapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	a.mu.Lock()
	a.starts = append(a.starts, spec)
	a.mu.Unlock()
	if a.StartFunc == nil {
		return backend.StartResult{}
	}
	return a.StartFunc(ctx, spec)
}

// Close records the reference and returns the scripted result.
func (a *Adapter) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	a.mu.Lock()
	a.closes = append(a.closes, ref)
	a.mu.Unlock()
	if a.CloseFunc == nil {
		return backend.CloseResult{}
	}
	return a.CloseFunc(ctx, ref)
}

// Probe records the reference and returns the scripted result.
func (a *Adapter) Probe(ctx context.Context, ref backend.Ref) backend.ProbeResult {
	a.mu.Lock()
	a.probes = append(a.probes, ref)
	a.mu.Unlock()
	if a.ProbeFunc == nil {
		return backend.ProbeResult{}
	}
	return a.ProbeFunc(ctx, ref)
}

// Starts returns the specs this adapter was handed, in order.
func (a *Adapter) Starts() []backend.StartSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]backend.StartSpec(nil), a.starts...)
}

// Closes returns the references Close was called with, in order. Its length is
// how a test proves cleanup ran exactly once — or not at all.
func (a *Adapter) Closes() []backend.Ref {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]backend.Ref(nil), a.closes...)
}

// Probes returns the references Probe was called with, in order.
func (a *Adapter) Probes() []backend.Ref {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]backend.Ref(nil), a.probes...)
}

// StartOnly is an adapter that can start but cannot clean up. It exists so a
// test can prove the capability preflight refuses before anything is created,
// which is not something the full fake can demonstrate.
type StartOnly struct{ kind backend.Kind }

// NewStartOnly builds an adapter missing the close and probe capabilities.
func NewStartOnly(kind backend.Kind) *StartOnly { return &StartOnly{kind: kind} }

// Kind reports the backend this fake stands in for.
func (a *StartOnly) Kind() backend.Kind { return a.kind }

// Start always reports an internal failure: this adapter exists to be refused
// by the capability preflight, so reaching Start at all is the defect.
func (a *StartOnly) Start(context.Context, backend.StartSpec) backend.StartResult {
	return backend.NewNotMutated(
		backend.NewStartCause(backend.FailureInternal, nil),
	)
}
