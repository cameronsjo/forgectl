// Package httpsrv is minimal shared scaffolding for forgectl's loopback-bound
// HTTP tools: a bind helper, a Host-header allowlist, and an optional
// bearer-token check. It is deliberately small — issue #76 Phase B (a
// general-purpose local HTTP server for forgectl) hasn't landed and its
// contract isn't frozen, so this package doesn't guess at that shape. What it
// owns today is only what `forgectl docs serve` (#93) needs: the bind
// address default and the two security gates a loopback server needs
// regardless of which command opens the socket.
//
// Loopback-vs-token is caller-supplied policy, not a package invariant: a
// caller decides whether to wire BearerToken at all, and under what
// condition (e.g. only when binding off 127.0.0.1). This package never makes
// that decision for them.
package httpsrv

import (
	"net"
	"net/http"
	"strings"
)

// LoopbackAddr is the safe zero-value default bind address: loopback-only
// with an OS-assigned port, so the listener is unreachable from any other
// host by construction. Callers that need a fixed port set one explicitly.
const LoopbackAddr = "127.0.0.1:0"

// DefaultAllowedHosts is the Host-header allowlist a loopback-bound server
// should apply regardless of bind address: 127.0.0.1, localhost, and ::1.
// Loopback bind alone does not stop DNS rebinding — a page open in the
// user's browser can resolve an attacker-controlled hostname to 127.0.0.1
// and then issue same-origin requests that still carry that hostname as the
// Host header. HostAllowlist is the second gate that catches those.
var DefaultAllowedHosts = []string{"127.0.0.1", "localhost", "::1"}

// Listen binds a TCP listener on addr. Kept as a named seam — rather than a
// bare net.Listen call at each call site — so bind policy has one place to
// grow (e.g. a future unix-socket mode) without a call-site ripple.
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// HostAllowlist returns middleware that responds 403 to any request whose
// Host header (port stripped, compared case-insensitively) is not in
// allowed. Applying it ahead of every other handler means a rebound request
// never reaches file-serving or rendering logic at all — the allowlist is
// item 1 of forgectl#93's security chain.
func HostAllowlist(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, h := range allowed {
		set[strings.ToLower(h)] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			host = strings.Trim(host, "[]") // bare IPv6 hosts arrive bracketed only when a port follows
			if !set[strings.ToLower(host)] {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BearerToken returns middleware that responds 401 to any request whose
// Authorization header isn't exactly "Bearer "+token. An empty token is
// refused at construction time by requiring callers to check for one before
// wiring this in — BearerToken has no "auth optional" mode of its own, so a
// caller that doesn't need auth simply never adds this middleware to its
// chain.
func BearerToken(token string) func(http.Handler) http.Handler {
	want := "Bearer " + token
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Chain composes middleware around h in the given order — mw[0] is
// outermost, so it sees a request first and a response last.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
