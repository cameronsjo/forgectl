package docs

// Test plan for discovery.go
//
// WriteServerInfo / ReadServerInfo (Classification: security-sensitive persistence — atomic write + liveness check)
//   [x] Happy: WriteServerInfo then ReadServerInfo round-trips Addr/Token/PID for a live listener
//   [x] Happy: WriteServerInfo creates the file 0600 (the file can carry a bearer token)
//   [x] Unhappy: ReadServerInfo on a missing file returns ErrNoServer
//   [x] Unhappy (security): ReadServerInfo on a file naming an address nothing is
//              listening on returns ErrNoServer — a stale PID must never be
//              trusted, only a live dial
//   [x] Unhappy: ReadServerInfo on a corrupt/unparseable file returns a
//              distinct wrapped error, not ErrNoServer
//   [x] Unhappy: ReadServerInfo on a file with an empty addr returns a
//              distinct wrapped error, not ErrNoServer
//
// RemoveServerInfo (Classification: idempotent cleanup)
//   [x] Happy: removes an existing file
//   [x] Happy: is a no-op (nil error) when the file doesn't exist
//
// dialable (Classification: liveness probe)
//   [x] Happy: true for an address something is listening on
//   [x] Unhappy: false for an address nothing is listening on
//
// ServerInfo.DocURL / BaseURL (Classification: URL construction)
//   [x] Happy: DocURL builds /doc/{root}/{relPath} against info.Addr
//   [x] Happy: DocURL escapes a path segment containing a space and a '#', so
//              neither can truncate or reshape the resulting URL
//   [x] Happy: BaseURL builds the index page URL
//
// LocateDoc (Classification: HTTP client — talks to the locate endpoint)
//   [x] Happy: 200 decodes into (root, rel, nil)
//   [x] Happy: a stored token is sent as an Authorization: Bearer header
//   [x] Unhappy: 404 returns ErrNotServed
//   [x] Unhappy: 401 returns an error naming a restart

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freeListener binds a loopback listener on an OS-assigned port and closes it
// on cleanup. Used both as a "something is listening here" fixture and, after
// an explicit early Close, as a "nothing is listening here anymore" address.
func freeListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck // best-effort; may already be closed by the test body
	return ln
}

func TestWriteServerInfo_ReadServerInfo_RoundTrips(t *testing.T) {
	ln := freeListener(t)
	path := filepath.Join(t.TempDir(), "docs-server.json")
	want := ServerInfo{Addr: ln.Addr().String(), Token: "s3cret", PID: 4242}

	if err := WriteServerInfo(path, want); err != nil {
		t.Fatalf("WriteServerInfo: %v", err)
	}
	got, err := ReadServerInfo(path)
	if err != nil {
		t.Fatalf("ReadServerInfo: %v", err)
	}
	if got != want {
		t.Errorf("ReadServerInfo = %+v, want %+v", got, want)
	}
}

func TestWriteServerInfo_FileMode0600(t *testing.T) {
	ln := freeListener(t)
	path := filepath.Join(t.TempDir(), "docs-server.json")

	if err := WriteServerInfo(path, ServerInfo{Addr: ln.Addr().String()}); err != nil {
		t.Fatalf("WriteServerInfo: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want %o — the discovery file can carry a bearer token and must never be group/world-readable", got, 0o600)
	}
}

func TestReadServerInfo_MissingFile_ErrNoServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	_, err := ReadServerInfo(path)
	if !errors.Is(err, ErrNoServer) {
		t.Errorf("ReadServerInfo on a missing file: err = %v, want ErrNoServer", err)
	}
}

func TestReadServerInfo_AddrNotDialable_ErrNoServer(t *testing.T) {
	ln := freeListener(t)
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "docs-server.json")
	if err := WriteServerInfo(path, ServerInfo{Addr: addr, PID: 999999}); err != nil {
		t.Fatalf("WriteServerInfo: %v", err)
	}

	_, err := ReadServerInfo(path)
	if !errors.Is(err, ErrNoServer) {
		t.Errorf("ReadServerInfo naming a dead address: err = %v, want ErrNoServer — a stale PID must not be trusted, only a live dial", err)
	}
}

func TestReadServerInfo_CorruptFile_DistinctFromErrNoServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docs-server.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadServerInfo(path)
	if err == nil {
		t.Fatal("ReadServerInfo on a corrupt file: got nil error, want one")
	}
	if errors.Is(err, ErrNoServer) {
		t.Errorf("ReadServerInfo on a corrupt file returned ErrNoServer, want a distinct parse error — a corrupt file is a different failure mode than a genuinely absent server")
	}
}

func TestReadServerInfo_EmptyAddr_DistinctFromErrNoServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docs-server.json")
	if err := os.WriteFile(path, []byte(`{"addr":"","pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ReadServerInfo(path)
	if err == nil {
		t.Fatal("ReadServerInfo with an empty addr: got nil error, want one")
	}
	if errors.Is(err, ErrNoServer) {
		t.Errorf("ReadServerInfo with an empty addr returned ErrNoServer, want a distinct validation error")
	}
}

func TestRemoveServerInfo_ExistingFile_Removed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docs-server.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RemoveServerInfo(path); err != nil {
		t.Fatalf("RemoveServerInfo: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still present after RemoveServerInfo, Stat err = %v", err)
	}
}

func TestRemoveServerInfo_MissingFile_NoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-existed.json")

	if err := RemoveServerInfo(path); err != nil {
		t.Errorf("RemoveServerInfo on a missing file: err = %v, want nil — shutdown must be idempotent", err)
	}
}

func TestDialable_ListeningAddr_True(t *testing.T) {
	ln := freeListener(t)

	if !dialable(ln.Addr().String()) {
		t.Errorf("dialable(%q) = false, want true — something is listening there", ln.Addr().String())
	}
}

func TestDialable_NothingListening_False(t *testing.T) {
	ln := freeListener(t)
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if dialable(addr) {
		t.Errorf("dialable(%q) = true, want false — nothing is listening there anymore", addr)
	}
}

func TestServerInfo_DocURL_BuildsPath(t *testing.T) {
	info := ServerInfo{Addr: "127.0.0.1:3590"}

	got := info.DocURL("docs", "plan.md")
	want := "http://127.0.0.1:3590/doc/docs/plan.md"
	if got != want {
		t.Errorf("DocURL = %q, want %q", got, want)
	}
}

func TestServerInfo_DocURL_EscapesSpecialChars(t *testing.T) {
	info := ServerInfo{Addr: "127.0.0.1:3590"}

	got := info.DocURL("docs", "release notes #3.md")
	if strings.Contains(got, " ") {
		t.Errorf("DocURL = %q, contains a literal space — an unescaped space can truncate the path a browser sends", got)
	}
	if strings.Contains(got, "#3.md") {
		t.Errorf("DocURL = %q, contains a literal '#' — unescaped, it would truncate the path into a URL fragment", got)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("DocURL produced an unparseable URL %q: %v", got, err)
	}
	if u.Path != "/doc/docs/release notes #3.md" {
		t.Errorf("parsed URL path = %q, want the original filename preserved through escaping", u.Path)
	}
}

func TestServerInfo_BaseURL_BuildsIndexURL(t *testing.T) {
	info := ServerInfo{Addr: "127.0.0.1:3590"}

	got := info.BaseURL()
	want := "http://127.0.0.1:3590/"
	if got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

func TestLocateDoc_OK_DecodesRootAndRel(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(locateResponse{Root: "docs", Rel: "plan.md", Title: "Plan"}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer ts.Close()

	info := ServerInfo{Addr: ts.Listener.Addr().String()}
	root, rel, err := LocateDoc(info, "/abs/plan.md")
	if err != nil {
		t.Fatalf("LocateDoc: %v", err)
	}
	if root != "docs" || rel != "plan.md" {
		t.Errorf("LocateDoc = (%q, %q), want (%q, %q)", root, rel, "docs", "plan.md")
	}
	if gotPath != "/abs/plan.md" {
		t.Errorf("server received path query = %q, want %q", gotPath, "/abs/plan.md")
	}
}

func TestLocateDoc_TokenSet_SendsBearerHeader(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewEncoder(w).Encode(locateResponse{Root: "docs", Rel: "plan.md"}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer ts.Close()

	info := ServerInfo{Addr: ts.Listener.Addr().String(), Token: "s3cret"}
	if _, _, err := LocateDoc(info, "/abs/plan.md"); err != nil {
		t.Fatalf("LocateDoc: %v", err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer s3cret")
	}
}

func TestLocateDoc_NotFound_ReturnsErrNotServed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	info := ServerInfo{Addr: ts.Listener.Addr().String()}
	_, _, err := LocateDoc(info, "/abs/missing.md")
	if !errors.Is(err, ErrNotServed) {
		t.Errorf("LocateDoc on a 404: err = %v, want ErrNotServed", err)
	}
}

func TestLocateDoc_Unauthorized_ErrorNamesRestart(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	info := ServerInfo{Addr: ts.Listener.Addr().String(), Token: "stale"}
	_, _, err := LocateDoc(info, "/abs/plan.md")
	if err == nil {
		t.Fatal("LocateDoc on a 401: got nil error, want one")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("error = %q, want it to tell the operator to restart the server rather than leaving a bare 401", err.Error())
	}
}
