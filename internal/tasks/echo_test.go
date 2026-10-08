package tasks

// Operator-config echoes (#778 item 4): the host, the base URL built from it,
// and the cache path are quoted and capped wherever an error names them.
//
//   [x] AssertVikunja's refusal quotes and caps the base URL
//   [x] An unresolvable host is quoted and capped, and so is the resolver's
//       own error, which names the host again
//   [x] SaveCache quotes the path and the PathError it wraps, and keeps the
//       PathError on the chain
//   [x] A failed request's transport error (a *url.Error, whose dial or DNS
//       text is uncapped) is escaped and capped (#807)
//   [x] A failed body read's error is escaped and capped the same way (#810)
//   [x] A dial address the pin cannot parse is quoted and capped (#810)

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestAssertVikunja_QuotesAndCapsTheBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer srv.Close()

	// U+202E is the one hostile rune a URL path can still carry to the
	// server; a long tail proves the cap.
	long := strings.Repeat("p", 200)
	client := NewClientForTesting(srv.URL+"/\u202e"+long, newToken(fakeToken))
	err := client.AssertVikunja(context.Background())
	if err == nil {
		t.Fatal("AssertVikunja against HTML = nil, want a refusal")
	}
	if strings.ContainsRune(err.Error(), '\u202e') {
		t.Errorf("the refusal carries a raw bidi override: %q", err)
	}
	if strings.Contains(err.Error(), long[:100]) || !strings.Contains(err.Error(), "…") {
		t.Errorf("the refusal echoes the base URL uncapped: %q", err)
	}
}

func TestCheckHostPinning_QuotesAnUnresolvableHost(t *testing.T) {
	host := "evil\x1b]0;pwned\x07" + strings.Repeat("h", 300) + ".invalid"
	_, _, err := checkHostPinning(context.Background(), &exec.FakeRunner{}, host, nil)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("checkHostPinning = %v, want ErrUnreachable", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the refusal carries a raw control: %q", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("h", 250)) {
		t.Errorf("the refusal echoes the host uncapped: %q", err)
	}
}

func TestSaveCache_QuotesThePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "evil\x1b]0;pwned\x07")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent is a regular file, so MkdirAll fails with ENOTDIR.
	err := SaveCache(filepath.Join(blocker, "sub", "cache.json"), Snapshot{})
	if err == nil {
		t.Fatal("SaveCache under a regular file = nil, want an error")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control from the path: %q", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("the PathError fell off the chain: %v", err)
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestDo_CapsTheTransportError(t *testing.T) {
	long := strings.Repeat("d", 1000)
	client := NewClientForTesting("http://tasks.invalid", newToken(fakeToken))
	client.httpClient = &http.Client{Transport: failingTransport{err: errors.New("dial: \x1b]0;pwned\x07" + long)}}
	_, err := client.FetchTasks(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("FetchTasks = %v, want ErrUnreachable", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control: %q", err)
	}
	if strings.Contains(err.Error(), long[:400]) || !strings.HasSuffix(err.Error(), "[truncated]") {
		t.Errorf("the error echoes the transport error uncapped: %q", err)
	}
}

type failingBody struct{ err error }

func (f failingBody) Read([]byte) (int, error) { return 0, f.err }
func (failingBody) Close() error               { return nil }

type failingBodyTransport struct{ err error }

func (f failingBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: failingBody(f), Request: req, Header: http.Header{}}, nil
}

// Mutation: restore the raw %v of readErr and the control and the full tail
// reach the error.
func TestDo_CapsTheReadBodyError(t *testing.T) {
	long := strings.Repeat("r", 1000)
	client := NewClientForTesting("http://tasks.invalid", newToken(fakeToken))
	client.httpClient = &http.Client{Transport: failingBodyTransport{err: errors.New("read: \x1b]0;pwned\x07" + long)}}
	_, err := client.FetchTasks(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("FetchTasks = %v, want ErrUnreachable", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control: %q", err)
	}
	if strings.Contains(err.Error(), long[:400]) || !strings.HasSuffix(err.Error(), "[truncated]") {
		t.Errorf("the error echoes the read error uncapped: %q", err)
	}
}

// Mutation: restore the %q of addr and %v of err and both assertions fail:
// net's parse error repeats the address raw, control and full tail included.
func TestPinnedDialer_QuotesAndCapsAnUnparsableAddress(t *testing.T) {
	long := strings.Repeat("a", 1000)
	_, err := pinnedDialer(nil, "", nil)(context.Background(), "tcp", "evil\x1b]0;pwned\x07"+long)
	if !IsHostRefused(err) {
		t.Fatalf("dial = %v, want a host refusal", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the refusal carries a raw control: %q", err)
	}
	if strings.Contains(err.Error(), long[:300]) {
		t.Errorf("the refusal echoes the dial address uncapped: %q", err)
	}
}
