package httpsrv

// Test plan for httpsrv.go
//
// HostAllowlist (Classification: security gate — DNS-rebinding defense)
//   [x] Happy: an allowed bare host (no port) passes through
//   [x] Happy: an allowed host with a port passes through (port is stripped)
//   [x] Happy: an allowed bracketed IPv6 host with a port passes through
//   [x] Unhappy: a disallowed Host header is rejected 403 and never reaches next
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

import (
	"net/http"
	"net/http/httptest"
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
