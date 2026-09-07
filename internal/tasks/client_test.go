package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
