package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateTask_SendsPutWithTheBearerHeader(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 42, "title": "hello", "project_id": 7}`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	task, err := client.CreateTask(context.Background(), 7, "hello", "a description")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.ID != 42 {
		t.Fatalf("CreateTask returned id %d, want 42", task.ID)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %s, want PUT (Vikunja creates with PUT)", gotMethod)
	}
	if gotPath != "/projects/7/tasks" {
		t.Fatalf("path = %s, want /projects/7/tasks", gotPath)
	}
	if gotAuth != "Bearer "+fakeToken {
		t.Fatalf("Authorization header = %q, want the bearer token", gotAuth)
	}
	if gotBody["title"] != "hello" {
		t.Fatalf("body title = %v, want hello", gotBody["title"])
	}
}

func TestCreateTask_MapsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code": 1001, "message": "missing, malformed, expired or otherwise invalid token"}`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	_, err := client.CreateTask(context.Background(), 7, "hello", "")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("CreateTask on 401 = %v, want errors.Is(ErrUnauthorized)", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Fatal("a 401 must never also read as ErrUnreachable")
	}
}

func TestCreateTask_RefusesAnEmptyTitle(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	if _, err := client.CreateTask(context.Background(), 7, "   ", ""); err == nil {
		t.Fatal("CreateTask with a blank title = nil error, want a refusal")
	}
	if called {
		t.Fatal("CreateTask with a blank title reached the server — it must refuse locally")
	}
}

func TestAddComment_SendsPutToTheCommentRoute(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 9, "comment": "hi"}`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	c, err := client.AddComment(context.Background(), 42, "hi")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if c.ID != 9 {
		t.Fatalf("AddComment returned id %d, want 9", c.ID)
	}
	if gotMethod != http.MethodPut || gotPath != "/tasks/42/comments" {
		t.Fatalf("got %s %s, want PUT /tasks/42/comments", gotMethod, gotPath)
	}
}

func TestAddComment_RefusesAnEmptyBody(t *testing.T) {
	client := NewClientForTesting("http://127.0.0.1:1", newToken(fakeToken))
	if _, err := client.AddComment(context.Background(), 42, "  \n "); err == nil {
		t.Fatal("AddComment with a blank body = nil error, want a refusal")
	}
}

// TestAssertVikunja_FailsOnAnHTMLBody is the catch-all-200 guard. A reverse
// proxy that answers every unmatched host with a landing page returns 200 with
// HTML — indistinguishable from a healthy instance to anything that only reads
// the status code, and the credential would then be sent to that proxy on every
// later call.
func TestAssertVikunja_FailsOnAnHTMLBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta http-equiv="refresh" content="0;url=https://example.invalid/"></head></html>`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	err := client.AssertVikunja(context.Background())
	if err == nil {
		t.Fatal("AssertVikunja against an HTML catch-all = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("the refusal should name the missing version field, got: %v", err)
	}
}

func TestAssertVikunja_FailsOnJSONWithoutAVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status": "ok"}`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	if err := client.AssertVikunja(context.Background()); err == nil {
		t.Fatal("AssertVikunja on JSON with no version field = nil, want a refusal")
	}
}

func TestAssertVikunja_AcceptsARealInfoResponse(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version": "v2.5.0", "frontend_url": "https://example.invalid/"}`))
	}))
	defer srv.Close()

	client := NewClientForTesting(srv.URL, newToken(fakeToken))
	if err := client.AssertVikunja(context.Background()); err != nil {
		t.Fatalf("AssertVikunja against a real /info = %v, want nil", err)
	}
	if gotPath != "/info" {
		t.Fatalf("AssertVikunja hit %s, want /info", gotPath)
	}
}
