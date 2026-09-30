package tasks

// Operator-config echoes (#778 item 4): the host, the base URL built from it,
// and the cache path are quoted and capped wherever an error names them.
//
//   [x] AssertVikunja's refusal quotes and caps the base URL
//   [x] An unresolvable host is quoted and capped, and so is the resolver's
//       own error, which names the host again
//   [x] SaveCache quotes the path and the PathError it wraps, and keeps the
//       PathError on the chain

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
