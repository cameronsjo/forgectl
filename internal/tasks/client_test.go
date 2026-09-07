package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestClient_401ProducesUnauthorizedAndNeverDecodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vikunja returns a JSON OBJECT on error, never the array a task-list
		// caller expects. A decoder aimed at a slice must never reach this body.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code": 1001, "message": "invalid or expired token"}`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	_, err := client.FetchTasks(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("FetchTasks() on 401 = %v, want errors.Is(ErrUnauthorized)", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Fatal("a 401 must never also read as ErrUnreachable — the two are distinct verdicts")
	}
}

func TestClient_UnreachableIsDistinctFromUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // closed before any request lands: connection refused

	client := NewClientForTesting(addr, newToken(fakeToken))
	_, err := client.FetchTasks(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("FetchTasks() on connection refused = %v, want errors.Is(ErrUnreachable)", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatal("a connection failure must never also read as ErrUnauthorized")
	}
}

func TestClient_SuccessDecodesArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		if page == "1" || page == "" {
			_ = json.NewEncoder(w).Encode([]Task{{ID: 1, Title: "one"}, {ID: 2, Title: "two"}})
			return
		}
		_ = json.NewEncoder(w).Encode([]Task{})
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	got, err := client.FetchTasks(context.Background())
	if err != nil {
		t.Fatalf("FetchTasks(): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FetchTasks() returned %d tasks, want 2", len(got))
	}
}

func TestClient_SendsBearerHeaderNotArgv(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Label{})
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	if _, err := client.FetchLabels(context.Background()); err != nil {
		t.Fatalf("FetchLabels(): %v", err)
	}
	if want := "Bearer " + fakeToken; gotAuth != want {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, want)
	}
}

// TestNewClient_InstallsThePinnedDialer asserts the WIRING, not the dialer.
//
// This test exists because the dialer's own tests passed with the
// `DialContext: pinnedDialer(...)` line deleted from NewClient — measured,
// not hypothesised. A correct control that nothing routes traffic through is
// indistinguishable from no control, and the unit tests could not tell the
// difference. So: build a real Client and assert its transport carries a
// DialContext, which is the one observable proof the pin is on the path the
// bearer token actually takes.
func TestNewClient_InstallsThePinnedDialer(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "route" {
				return "gateway: " + HomelabGateway + "\n", nil
			}
			return "", nil
		},
	}
	// An IP literal resolves to itself, so this needs no DNS.
	c, err := NewClient(context.Background(), runner, "192.168.1.102", newToken("tk_"+strings.Repeat("a", 40)))
	if err != nil {
		t.Fatalf("NewClient = %v, want nil", err)
	}
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.httpClient.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("transport has no DialContext — the host pin is not on the request path, " +
			"so http.Transport will resolve the hostname itself and the pin constrains nothing")
	}
}

// TestFetchAllPages_StopsAtTheCap pins the bound on a loop whose termination
// was otherwise decided entirely by the server: a host returning a full page
// forever spun it and grew the result slice without limit.
func TestFetchAllPages_StopsAtTheCap(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pages++
		full := make([]map[string]any, pageSize) // always a FULL page: never terminates on its own
		_ = json.NewEncoder(w).Encode(full)
	}))
	defer srv.Close()

	c := NewClientForTesting(srv.URL, newToken("tk_"+strings.Repeat("a", 40)))
	_, err := fetchAllPages[map[string]any](context.Background(), c, "/tasks")
	if !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("fetchAllPages against a never-ending server = %v, want errors.Is(ErrUnexpectedStatus)", err)
	}
	if pages != maxPages {
		t.Fatalf("made %d requests, want exactly maxPages (%d)", pages, maxPages)
	}
}
