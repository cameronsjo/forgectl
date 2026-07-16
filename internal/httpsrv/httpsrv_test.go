package httpsrv

// Test plan for httpsrv.go
//
// HostAllowlist (Classification: security gate — DNS-rebinding defense)
//   [x] Happy: an allowed bare host (no port) passes through
//   [x] Happy: an allowed host with a port passes through (port is stripped)
//   [x] Happy: an allowed bracketed IPv6 host with a port passes through
//   [x] Unhappy: a disallowed Host header is rejected 403 and never reaches next
//
// BearerToken (Classification: security gate)
//   [x] Happy: the exact "Bearer <token>" header passes through
//   [x] Unhappy: a missing/wrong Authorization header is rejected 401
//
// Chain (Classification: helper)
//   [x] Happy: middleware runs outermost-first

import (
	"net/http"
	"net/http/httptest"
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
