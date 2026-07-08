// Package bench is forgectl's interop spine across the local developer bench —
// the hearth telemetry stack, the chronicle transcript-retention layer, and the
// flux board. It discovers and health-checks each system through its frozen
// interop contract; it never reimplements a service's own logic. All probing is
// injectable (exec.Runner for shell-outs, Prober for network reachability) so
// the orchestration stays pure and table-testable, and every component degrades
// to a named state — never a returned error — when a dependency is absent.
package bench

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// Prober checks a target for reachability. It is the network seam that keeps
// bench status pure and table-testable (mirroring the exec.Runner seam):
// production uses the httpProber, tests inject a fake.
//
// The target is a URL. An http/https target does a GET and returns the response
// status code. A tcp://host:port target does a bare TCP connect — used for
// hearth's OTLP gRPC transport, which is not HTTP-probeable — and returns
// (0, nil) on a successful connect (no HTTP status applies). Either scheme
// returns (0, err) when the endpoint cannot be reached.
type Prober interface {
	Probe(ctx context.Context, target string) (int, error)
}

// probeTimeout bounds a single reachability check so a dead endpoint never
// stalls the status card.
const probeTimeout = 2 * time.Second

// httpProber is the production Prober. This is the repo's only net/http caller,
// deliberately fenced behind the Prober interface.
type httpProber struct {
	client *http.Client
}

// NewHTTPProber returns the production Prober with a short per-probe timeout. A
// health probe never follows redirects — the status code of the first response
// is all it needs, and following a redirect off localhost would be surprising.
func NewHTTPProber() Prober {
	return httpProber{client: &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Probe dispatches on the target scheme: tcp:// does a bare connect, everything
// else an HTTP GET.
func (h httpProber) Probe(ctx context.Context, target string) (int, error) {
	if addr, ok := strings.CutPrefix(target, "tcp://"); ok {
		// Bound the connect explicitly: the caller's context often carries no
		// deadline, and a filtered (silently dropped) port would otherwise stall
		// on the OS SYN-retry window rather than probeTimeout.
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return 0, err
		}
		_ = conn.Close()
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
