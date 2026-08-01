// Package httpsrv is minimal shared scaffolding for forgectl's loopback-bound
// HTTP tools: a bind helper, a Host-header allowlist, a cross-site request
// rejecter, and an optional bearer-token check. It is deliberately small —
// issue #76 Phase B (a general-purpose local HTTP server for forgectl) hasn't
// landed and its contract isn't frozen, so this package doesn't guess at that
// shape. What it owns today is only what `forgectl docs serve` (#93) needs:
// the bind address default and the three security gates a loopback server
// needs regardless of which command opens the socket.
//
// Loopback-vs-token is caller-supplied policy, not a package invariant: a
// caller decides whether to wire BearerToken at all, and under what
// condition (e.g. only when binding off 127.0.0.1). This package never makes
// that decision for them.
package httpsrv

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
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

// RejectCrossSite returns middleware that responds 403 to any request the
// browser itself labels as cross-site — Sec-Fetch-Site: cross-site, meaning
// some other origin's page initiated it. The Host allowlist above stops a
// rebound hostname; this stops the plainer shape it does not cover, a page on
// the open web reaching a correctly-addressed 127.0.0.1 server through the
// operator's own browser. The same-origin policy stops that page READING the
// response, but the request is still DELIVERED, and delivery alone is enough
// to make the server work: index lookups run, and an unbounded number of
// /events subscribers can be opened and held.
//
// An ABSENT header ALLOWS the request, deliberately. Every non-browser client
// in this codebase omits Sec-Fetch-Site: the Go http.Client behind
// docspkg.LocateDoc, the curl command `forgectl docs open` prints for a
// token-protected reader (internal/cli/docs_open.go), and an operator's own
// curl. Denying on absence would break `forgectl docs open` outright.
//
// This gate is BEST-EFFORT, and the reason is the absent-allows rule above
// meeting a limit in how browsers emit the header. Fetch Metadata headers
// (Sec-Fetch-*) are attached ONLY to requests whose URL is potentially
// trustworthy. http://127.0.0.1:PORT qualifies under the localhost exception,
// so on the default loopback bind a browser always sends it. A non-loopback
// plain-HTTP bind — what --addr or [docs].addr produce for a LAN or Tailscale
// address — does NOT qualify, so the browser sends NOTHING and a genuinely
// cross-site request reaches this check indistinguishable from curl. Old
// Safari (before 16.4) omits the header everywhere. So in the highest-exposure
// mode this check is inert, and it must not be read as the control that makes
// an exposed bind safe: what covers that case is the bearer token
// resolveToken forces off loopback (internal/cli/docs_serve.go), which is
// caller-side policy this package deliberately does not own. Treat this as
// defense in depth for the loopback case, never as the boundary.
//
// The Origin check is the complement, and it is why both live here. Origin is
// a forbidden header name, so a page cannot forge or suppress it, and it rides
// every CORS-mode request and every non-GET request REGARDLESS of whether the
// URL is potentially trustworthy — which is exactly the gap above. It does not
// replace the Sec-Fetch-Site check, because a no-cors GET subresource load (an
// <img> or a no-cors fetch, the cheapest cross-site reach) carries no Origin at
// all. Each covers what the other misses; keep both.
//
// Comparing Origin against this request's OWN scheme+host is only sound
// because HostAllowlist runs upstream. Without it, a rebinding attacker
// controls the Host header, so the attacker's Origin and the reconstructed
// origin would agree and the comparison would pass. The allowlist is what
// pins r.Host to a value the operator sanctioned; do not wire this middleware
// without it.
//
// same-site is NOT rejected, and that is a chosen non-goal. A browser's notion
// of "site" excludes the port, so another loopback listener — a page served
// from 127.0.0.1:9999 — reaches this server labeled same-site and passes.
// Closing that gap buys little: an attacker who can already serve pages from
// this machine's loopback interface is past the boundary the loopback bind and
// the Host allowlist defend. The scope here is the remote web page, not the
// local one.
//
// There is deliberately NO carve-out for top-level navigation. The canonical
// Fetch Metadata resource-isolation policy allows a cross-site request when it
// is a GET navigation to a document, on the theory that following a link
// should work. This reader rejects it: nothing off-site has a reason to link
// into a local docs server, and a 403 on a navigation is a visible, harmless
// failure while the alternative keeps the class half-open. `forgectl docs
// open` and --open are unaffected — an OS-level browser open is not a
// navigation FROM a page, so it arrives as Sec-Fetch-Site: none.
//
// It closes the CLASS rather than one route. Applied around the whole handler,
// a future mutating endpoint or a later CORS response header inherits the
// rejection without its author having to remember this exists.
func RejectCrossSite() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A present-but-foreign Origin is the strongest signal available and
			// the one that survives a non-trustworthy URL. "null" (a sandboxed
			// iframe, some redirect chains) is not this server's origin either,
			// so it lands here too — the safe direction.
			if origin := r.Header.Get("Origin"); origin != "" && !strings.EqualFold(origin, requestOrigin(r)) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			// An absent header is the empty string, which is never equal-fold to
			// "cross-site" — so "present AND cross-site" is the whole condition.
			if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requestOrigin reconstructs the origin a browser would serialize for a page
// served by THIS request's own URL — scheme://host[:port] — so an Origin header
// can be compared against it.
//
// The scheme comes from r.TLS rather than any X-Forwarded-Proto: forgectl
// serves plain HTTP and terminates its own connections, so the connection's own
// state is the truth, and trusting a forwarded header would let a client pick
// the scheme it is compared against.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// BearerToken returns middleware that responds 401 to any request whose
// Authorization header isn't exactly "Bearer "+token. An empty token is
// refused at construction time by requiring callers to check for one before
// wiring this in — BearerToken has no "auth optional" mode of its own, so a
// caller that doesn't need auth simply never adds this middleware to its
// chain.
//
// The comparison hashes both the expected and presented values (SHA-256)
// before running subtle.ConstantTimeCompare on the two fixed-size digests,
// rather than comparing the raw header bytes directly. Two reasons: a plain
// `!=`/bytes.Equal short-circuits on the first differing byte, leaking the
// correct token one byte at a time to an attacker who can measure response
// timing; and ConstantTimeCompare itself returns immediately when its inputs
// have different lengths, which would leak the token's length if compared
// raw. Hashing first makes both sides always exactly 32 bytes, so the compare
// step reveals nothing about the token's content or length.
//
// The guarantee is scoped to the SECRET, not to total request time: SHA-256
// cost scales with input size, so a caller can still learn how long its own
// header was from how long hashing it took. That is not a leak — the attacker
// supplied that header and already knows its length. What no measurement
// reveals is any property of the expected token, which is the only thing being
// protected here.
func BearerToken(token string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte("Bearer " + token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// IsLoopbackAddr reports whether addr's host is a loopback address, i.e.
// whether binding it exposes the server ONLY to this machine.
//
// It exists so a caller can make "is this exposed to the network?" an explicit
// decision instead of a guess. Anything it cannot confidently classify as
// loopback — an unparseable address, a hostname, the empty host that means
// "bind every interface" — is reported as NOT loopback. That direction of
// failure is the safe one: a caller using this to decide whether to require
// authentication must err toward requiring it, never toward skipping it.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port, or malformed. Try the whole string as a bare host rather
		// than assuming.
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false // ":3590" binds every interface, including public ones
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // a hostname we cannot resolve here; assume exposed
	}
	return ip.IsLoopback()
}

// GenerateToken returns a cryptographically random bearer token, hex-encoded.
// Used when a caller binds off loopback and therefore needs authentication it
// was not given explicitly.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Chain composes middleware around h in the given order — mw[0] is
// outermost, so it sees a request first and a response last.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
