package quarantine

// The strip step's error echoes (#807 item 1). A workflow glob and the
// cloned repo's file names are both remote-controlled, so every error that
// names one quotes and caps it:
//
//   [x] a rejected glob (absolute, root, traversal) is quoted and capped
//   [x] a bad pattern is quoted and capped, including globFold's own echo
//   [x] a failed removal quotes the glob and the PathError's path, and keeps
//       the PathError on the chain
//   [x] a match escaping the workspace is named quoted, relative to the
//       workspace root (#821)

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// hostile is a terminal control plus a marker; longTail proves a cap.
const hostile = "\x1b]0;pwned\x07"

var longTail = strings.Repeat("g", 300)

func assertQuotedAndCapped(t *testing.T, err error, tail string) {
	t.Helper()
	if err == nil {
		t.Fatal("strip = nil, want a refusal")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control: %q", err)
	}
	if strings.Contains(err.Error(), tail[:100]) {
		t.Errorf("the error echoes the glob uncapped: %q", err)
	}
}

func TestStrip_RejectedGlobIsQuotedAndCapped(t *testing.T) {
	for _, g := range []string{
		"/abs" + hostile + longTail,
		"../" + hostile + longTail,
		"./" + hostile + longTail + "/..",
	} {
		assertQuotedAndCapped(t, validateStripGlob(g), longTail)
	}
}

func TestStrip_BadPatternIsQuotedAndCapped(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runStrip(t, &exec.FakeRunner{}, workspace, []string{hostile + longTail + "["})
	assertQuotedAndCapped(t, err, longTail)
	if !errors.Is(err, filepath.ErrBadPattern) {
		t.Errorf("ErrBadPattern fell off the chain: %v", err)
	}
}

func TestStrip_RemoveFailureIsQuotedAndCapped(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "mcp.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := removeStripTarget
	removeStripTarget = func(target string) error {
		return &os.PathError{Op: "unlinkat", Path: filepath.Join(target, "evil"+hostile), Err: syscall.EIO}
	}
	t.Cleanup(func() { removeStripTarget = prev })

	// The class matches the "m" and the run of stars matches nothing, so the
	// glob reaches the removal with both controls and a long tail in it.
	// (hostile holds a ']', which would close the class early.)
	stars := strings.Repeat("*", 300)
	err := runStrip(t, &exec.FakeRunner{}, workspace, []string{"[m\x1b\x07]" + stars + "cp.json"})
	assertQuotedAndCapped(t, err, stars)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || !errors.Is(err, syscall.EIO) {
		t.Errorf("the PathError fell off the chain: %v", err)
	}
}

func TestStrip_EscapingMatchIsQuoted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(workspace, "evil"+hostile)); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	err := runStrip(t, &exec.FakeRunner{}, workspace, []string{"evil*"})
	if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("strip through an outward symlink = %v, want an escape refusal", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the error carries a raw control: %q", err)
	}
	// #821: the match is named relative to the workspace root, not by the
	// operator's absolute workspace path.
	if strings.Contains(err.Error(), workspace) {
		t.Errorf("the error discloses the absolute workspace path: %q", err)
	}
	if want := termsafe.QuotePath("evil" + hostile); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the match as %s", err, want)
	}
}

func TestStripMatchEcho_FallsBackOutsideTheWorkspace(t *testing.T) {
	ws := filepath.Join(string(filepath.Separator), "ws")
	if got, want := stripMatchEcho(ws, filepath.Join(ws, "a", "b")), termsafe.QuotePath(filepath.Join("a", "b")); got != want {
		t.Errorf("inside: got %s, want %s", got, want)
	}
	outside := filepath.Join(string(filepath.Separator), "elsewhere", "x")
	if got, want := stripMatchEcho(ws, outside), termsafe.QuotePath(outside); got != want {
		t.Errorf("outside: got %s, want %s", got, want)
	}
}
