package httpsrv

// Test plan for httpsrv.go
//
// HostAllowlist (Classification: security gate — DNS-rebinding defense)
//   [x] Happy: an allowed bare host (no port) passes through
//   [x] Happy: an allowed host with a port passes through (port is stripped)
//   [x] Happy: an allowed bracketed IPv6 host with a port passes through
//   [x] Unhappy: a disallowed Host header is rejected 403 and never reaches next
//
// RejectCrossSite (Classification: security gate — cross-site request defense)
//   [x] Unhappy (security): Sec-Fetch-Site: cross-site is rejected 403 and
//       never reaches next
//   [x] Unhappy (security): a mixed-case "Cross-Site" value is rejected too —
//       the comparison is case-insensitive, not a literal match
//   [x] Happy: same-origin, same-site, and none all pass through — the three
//       legitimate values a browser sends for the reader's own pages
//   [x] Happy (LOAD-BEARING): an absent header passes through. This is what
//       keeps every non-browser client working — `forgectl docs open`'s Go
//       http.Client, the curl hint, an operator's own curl — and a
//       deny-on-absent regression would break the command outright
//
// BearerToken (Classification: security gate — timing-safe comparison)
//   [x] Happy: the exact "Bearer <token>" header passes through
//   [x] Unhappy: a missing/wrong Authorization header is rejected 401
//   [x] Unhappy: a header sharing a long common prefix with the real token
//       (the classic byte-at-a-time timing-leak shape) is still rejected
//   [x] Unhappy: headers shorter and longer than the real token are both
//       rejected — the hash-first comparison must not special-case length
//
// Chain (Classification: helper)
//   [x] Happy: middleware runs outermost-first
//
// IsLoopbackAddr (Classification: security gate — exposure classification,
//              fails toward "exposed" on anything unclear)
//   [x] Happy: 127.0.0.1:3590, 127.0.0.1, localhost:80, [::1]:3590, ::1, and
//              another 127.x.x.x address are all loopback
//   [x] Unhappy (security): 0.0.0.0:3590, ":3590" (every interface), a LAN
//              address, a Tailscale-shaped 100.x.y.z address, an arbitrary
//              hostname, and malformed input are all NOT loopback — the bias
//              toward false is deliberate: a caller uses this to decide
//              whether to REQUIRE authentication, so anything unclassifiable
//              must read as exposed, never as safe
//
// GenerateToken (Classification: secret generation)
//   [x] Happy: returns a 64-hex-char (32-byte) token
//   [x] Happy: two calls return different tokens

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestHostAllowlist_AllowedBareHost_PassesThrough(t *testing.T) {
	h := HostAllowlist(DefaultAllowedHosts)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHostAllowlist_AllowedHostWithPort_PassesThrough(t *testing.T) {
	h := HostAllowlist(DefaultAllowedHosts)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:4712"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHostAllowlist_AllowedBracketedIPv6WithPort_PassesThrough(t *testing.T) {
	h := HostAllowlist(DefaultAllowedHosts)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "[::1]:4712"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHostAllowlist_SpoofedHost_Rejected403(t *testing.T) {
	called := false
	h := HostAllowlist(DefaultAllowedHosts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if called {
		t.Error("next handler was called for a spoofed Host header")
	}
}

func TestRejectCrossSite_CrossSite_Rejected403(t *testing.T) {
	called := false
	h := RejectCrossSite()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	// The status alone would still be satisfied by a handler that did the work
	// and then overwrote the code. What this gate promises is that a cross-site
	// request performs no work at all.
	if called {
		t.Error("next handler was called for a request the browser labeled cross-site")
	}
}

// TestRejectCrossSite_MixedCaseValue_Rejected403 pins the case-insensitive
// comparison. Sec-Fetch-Site values are lowercase per the Fetch Metadata spec,
// so this is not a shape any browser sends — it is a guard against a future
// refactor swapping strings.EqualFold for a plain ==, which would turn the
// rejection into something a non-conforming client could sidestep by changing
// one letter.
func TestRejectCrossSite_MixedCaseValue_Rejected403(t *testing.T) {
	h := RejectCrossSite()(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "Cross-Site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d for a mixed-case cross-site value", rec.Code, http.StatusForbidden)
	}
}

// TestRejectCrossSite_SameOriginSameSiteAndNone_PassThrough covers the three
// values a browser attaches to the reader's own traffic: same-origin for the
// page's own asset loads and SSE stream, none for a request the user initiated
// directly (typing the URL, a bookmark), and same-site for another loopback
// port — which a browser labels same-site because a "site" excludes the port.
// That last one is the documented non-goal; this test is where the decision is
// recorded as intentional rather than missed.
func TestRejectCrossSite_SameOriginSameSiteAndNone_PassThrough(t *testing.T) {
	cases := []string{"same-origin", "same-site", "none"}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			h := RejectCrossSite()(okHandler())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Sec-Fetch-Site", value)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d for Sec-Fetch-Site: %s", rec.Code, http.StatusOK, value)
			}
		})
	}
}

// TestRejectCrossSite_AbsentHeader_PassesThrough is the load-bearing case.
// Every non-browser client omits Sec-Fetch-Site, so deny-on-absent is the one
// regression here that breaks working commands rather than merely tightening
// something — and it would break them the moment the fix shipped, not under
// some rare condition.
func TestRejectCrossSite_AbsentHeader_PassesThrough(t *testing.T) {
	h := RejectCrossSite()(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil) // no Sec-Fetch-Site at all
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d for a request with NO Sec-Fetch-Site header. Absent must ALLOW: no non-browser client sends this header, so rejecting on absence breaks `forgectl docs open` (its Go http.Client), the curl command docs open prints for a token-protected reader, and every operator curl. Browsers always send it, so absence is not the traffic this gate is aimed at", rec.Code, http.StatusOK)
	}
}

func TestBearerToken_ExactToken_PassesThrough(t *testing.T) {
	h := BearerToken("s3cret")(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestBearerToken_WrongOrMissingToken_Rejected401(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"wrong token", "Bearer nope"},
		{"missing scheme", "s3cret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := BearerToken("s3cret")(okHandler())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

// TestBearerToken_NearMissAndLengthVariants_AllRejected exercises the shapes
// a timing attacker would probe: a header that matches the real token for a
// long common prefix then diverges (byte-at-a-time guessing), and headers
// both shorter and longer than the real token (probing whether length alone
// short-circuits the comparison). All must reject identically — the
// hash-first comparison in BearerToken collapses every case to a fixed-size
// digest compare (crypto/subtle.ConstantTimeCompare over two SHA-256 sums),
// so there is nothing here for a length or prefix probe to distinguish. This
// is a correctness test, not a timing measurement — proving the comparison
// is actually constant-time needs a dedicated statistical harness, which a
// unit test can't reliably provide; what this pins is that the fix didn't
// change accept/reject behavior for any of these shapes.
func TestBearerToken_NearMissAndLengthVariants_AllRejected(t *testing.T) {
	// Named wantToken, not "real": `real` is a predeclared Go identifier (the
	// complex-number accessor), and shadowing it trips the predeclared linter.
	const wantToken = "correct-horse-battery-staple-0123456789"
	h := BearerToken(wantToken)(okHandler())

	cases := map[string]string{
		"prefix match, last byte wrong": "Bearer " + wantToken[:len(wantToken)-1] + "X",
		"prefix match, then truncated":  "Bearer " + wantToken[:len(wantToken)-5],
		"much shorter than real token":  "Bearer x",
		"much longer than real token":   "Bearer " + wantToken + strings.Repeat("Y", 100),
		"empty bearer value":            "Bearer ",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", header)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d for header %q", rec.Code, http.StatusUnauthorized, header)
			}
		})
	}
}

func TestChain_RunsOutermostFirst(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(okHandler(), mw("outer"), mw("inner"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(order) != 2 || order[0] != "outer" || order[1] != "inner" {
		t.Errorf("order = %v, want [outer inner]", order)
	}
}

func TestIsLoopbackAddr_LoopbackForms_True(t *testing.T) {
	cases := []string{"127.0.0.1:3590", "127.0.0.1", "localhost:80", "[::1]:3590", "::1", "127.5.5.5:80"}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			if !IsLoopbackAddr(addr) {
				t.Errorf("IsLoopbackAddr(%q) = false, want true", addr)
			}
		})
	}
}

// TestIsLoopbackAddr_NonLoopbackAndUnclassifiable_False pins the deliberate
// bias documented on IsLoopbackAddr: anything this function cannot confirm is
// loopback must come back false, never true. A caller wires authentication on
// that boolean, so the failure direction that matters is "wrongly calls
// something exposed loopback" — that's the one that would ship a server with
// no auth reachable off the box. This test asserts every shape that bias is
// meant to catch, not just the unambiguous ones.
func TestIsLoopbackAddr_NonLoopbackAndUnclassifiable_False(t *testing.T) {
	cases := []string{
		"0.0.0.0:3590",          // every interface
		":3590",                 // empty host = every interface
		"192.168.1.10:3590",     // LAN address
		"100.64.1.2:3590",       // Tailscale-shaped CGNAT range
		"example.internal:3590", // arbitrary hostname, not resolved here
		"::not-an-address::",    // malformed input
	}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			if IsLoopbackAddr(addr) {
				t.Errorf("IsLoopbackAddr(%q) = true, want false — a caller decides whether to require auth from this, and an unclassifiable or non-loopback address must read as exposed", addr)
			}
		})
	}
}

func TestGenerateToken_Returns64HexChars(t *testing.T) {
	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		t.Errorf("GenerateToken = %q, want 64 lowercase hex characters (32 random bytes)", token)
	}
}

func TestGenerateToken_TwoCalls_Differ(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if a == b {
		t.Errorf("two GenerateToken calls returned the same value %q, want independently random tokens", a)
	}
}
