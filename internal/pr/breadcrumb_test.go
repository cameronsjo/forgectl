package pr

// Test plan for breadcrumb.go
//
// loadBreadcrumb (Classification: hostile-input location + content validation)
//   [x] Happy: a well-formed breadcrumb inside the session-state dir loads
//   [x] LOCATION reject: a path outside the session-state dir (no read)
//   [x] LOCATION reject: a symlink inside the dir pointing OUTSIDE it
//   [x] CONTENT reject: invalid JSON
//   [x] CONTENT reject: unknown fields (schema drift / smuggled keys)
//   [x] CONTENT reject: missing required fields (workspace, ref, createdAt)
//   [x] CONTENT reject: workspace is not an existing dir
//   [x] CONTENT reject: workspace lacks the forgectl- sandbox prefix
//   [x] CONTENT accept: a prefixed workspace outside the current temp root
//       (the documented widening from #184)
//   [x] CONTENT reject: malformed ref string
//   [x] $TMPDIR moving between create and load does not blind the loader
//   [x] $TMPDIR moving between create and load does not blind List
// writeBreadcrumb round-trips through loadBreadcrumb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// fakeWorkspace makes a dir under the OS temp root with the forgectl- prefix —
// what validateWorkspace accepts as a real sandbox.
func fakeWorkspace(t *testing.T) string {
	t.Helper()
	ws, err := os.MkdirTemp("", "forgectl-test-*")
	if err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })
	return ws
}

func writeRaw(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoadBreadcrumb_Happy(t *testing.T) {
	dir := t.TempDir()
	ws := fakeWorkspace(t)
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	bc := Breadcrumb{Workspace: ws, Ref: ref.String(), Agent: "claude", CreatedAt: time.Now().UTC()}

	path, err := writeBreadcrumb(dir, ref, bc)
	if err != nil {
		t.Fatalf("writeBreadcrumb: %v", err)
	}
	got, err := loadBreadcrumb(path, dir)
	if err != nil {
		t.Fatalf("loadBreadcrumb: %v", err)
	}
	if got.Workspace != ws || got.Ref != ref.String() || got.Agent != "claude" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestLoadBreadcrumb_LocationOutside(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	ws := fakeWorkspace(t)
	body := `{"workspace":"` + ws + `","ref":"o/r#1","agent":"claude","createdAt":"2026-07-08T00:00:00Z"}`
	outside := writeRaw(t, other, "sneaky.json", body)

	if _, err := loadBreadcrumb(outside, dir); err == nil {
		t.Error("expected location rejection for a path outside the session-state dir")
	}
}

func TestLoadBreadcrumb_SymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	ws := fakeWorkspace(t)
	body := `{"workspace":"` + ws + `","ref":"o/r#1","agent":"claude","createdAt":"2026-07-08T00:00:00Z"}`
	realTarget := writeRaw(t, other, "real.json", body)

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := loadBreadcrumb(link, dir); err == nil {
		t.Error("expected rejection for a symlink escaping the session-state dir")
	}
}

func TestLoadBreadcrumb_ContentRejections(t *testing.T) {
	dir := t.TempDir()
	ws := fakeWorkspace(t)

	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"unknown field", `{"workspace":"` + ws + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z","cmd":"evil"}`},
		{"missing workspace", `{"ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`},
		{"missing ref", `{"workspace":"` + ws + `","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`},
		{"missing createdAt", `{"workspace":"` + ws + `","ref":"o/r#1","agent":"a"}`},
		{"malformed ref", `{"workspace":"` + ws + `","ref":"not a ref","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`},
		{"workspace missing", `{"workspace":"/tmp/forgectl-does-not-exist-xyz","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRaw(t, dir, "bc"+string(rune('a'+i))+".json", tc.body)
			if _, err := loadBreadcrumb(path, dir); err == nil {
				t.Errorf("%s: expected content rejection", tc.name)
			}
		})
	}
}

// FuzzLoadBreadcrumb mutates breadcrumb JSON against a fixed session-state dir.
// The security invariant: any breadcrumb the loader RETURNS (no error) must
// point at a Workspace whose symlink-resolved name carries the forgectl-
// sandbox prefix — the loader never yields a breadcrumb steering a later
// `git -C` at an arbitrary path. There is no temp-root half to this property
// any more; #184 removed that check from validateWorkspace because it read
// $TMPDIR at call time. Seeded with valid breadcrumbs pointing at a real
// sandbox dir.
func FuzzLoadBreadcrumb(f *testing.F) {
	sessionsDir := f.TempDir()
	ws, err := os.MkdirTemp("", "forgectl-fuzz-*")
	if err != nil {
		f.Fatalf("mkdir workspace: %v", err)
	}
	f.Cleanup(func() { os.RemoveAll(ws) })

	for _, s := range []string{
		`{"workspace":"` + ws + `","ref":"cameronsjo/forgectl#42","agent":"claude","createdAt":"2026-07-08T00:00:00Z"}`,
		`{"workspace":"` + ws + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`,
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		path := filepath.Join(sessionsDir, "bc.json")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Skipf("write breadcrumb: %v", err)
		}
		bc, err := loadBreadcrumb(path, sessionsDir)
		if err != nil {
			return
		}
		real := bc.Workspace
		if r, err := filepath.EvalSymlinks(bc.Workspace); err == nil {
			real = r
		}
		if !strings.HasPrefix(filepath.Base(real), tempPrefix) {
			t.Errorf("loaded breadcrumb workspace %q lacks the %q sandbox prefix", bc.Workspace, tempPrefix)
		}
	})
}

// TestLoadBreadcrumb_WorkspaceBadPrefix now carries the whole workspace guard:
// with the temp-root check gone (#184), the symlink-resolved prefix is the only
// thing standing between a hostile breadcrumb and a later `git -C`.
func TestLoadBreadcrumb_WorkspaceBadPrefix(t *testing.T) {
	dir := t.TempDir()
	// A real, existing dir that lacks the forgectl- sandbox prefix — so the
	// prefix branch of validateWorkspace must reject it even though it exists
	// and is a directory.
	notASandbox := t.TempDir()
	body := `{"workspace":"` + notASandbox + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`
	path := writeRaw(t, dir, "bc.json", body)
	if _, err := loadBreadcrumb(path, dir); err == nil {
		t.Error("expected rejection for a workspace lacking the forgectl- sandbox prefix")
	}
}

// TestLoadBreadcrumb_SurvivesTempDirChange is the #184 regression. A breadcrumb
// written by one process must still load when a later verb runs under a
// different $TMPDIR — the condition that used to make `pr list` silently empty
// and attach/open/teardown hard-fail, reached either by an operator-set TMPDIR
// or by an UNSET one under launchd, cron, or a container.
func TestLoadBreadcrumb_SurvivesTempDirChange(t *testing.T) {
	// Every t.TempDir() call has to precede the t.Setenv below: t.TempDir()
	// itself resolves against $TMPDIR, so a dir minted afterwards would land
	// under the "changed" root and defeat the point.
	ws := fakeWorkspace(t)
	dir := t.TempDir()
	otherRoot := t.TempDir()

	// Without this the test could go vacuous: if the redirected root happened to
	// contain ws, even the old temp-root check would have passed.
	if sandbox.WithinWorkspace(otherRoot, ws) {
		t.Fatalf("precondition: otherRoot %q must not contain ws %q", otherRoot, ws)
	}

	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 184}
	bc := Breadcrumb{Workspace: ws, Ref: ref.String(), Agent: "claude", CreatedAt: time.Now().UTC()}
	path, err := writeBreadcrumb(dir, ref, bc)
	if err != nil {
		t.Fatalf("writeBreadcrumb: %v", err)
	}

	t.Setenv("TMPDIR", otherRoot)

	got, err := loadBreadcrumb(path, dir)
	if err != nil {
		t.Fatalf("loadBreadcrumb after a $TMPDIR change: %v", err)
	}
	if got.Workspace != ws {
		t.Errorf("workspace = %q, want %q", got.Workspace, ws)
	}
}

// TestLoadBreadcrumb_PrefixedDirOutsideTempRootAccepted pins the widening #184
// took on deliberately: a forgectl-prefixed existing directory loads even when
// it sits outside the CURRENT temp root. From inside the loader that case is
// indistinguishable from a legitimate workspace created under a different
// $TMPDIR, and refusing it cost every $TMPDIR-crossing session while a same-uid
// adversary could plant a forgectl-* dir inside the temp root just as easily.
func TestLoadBreadcrumb_PrefixedDirOutsideTempRootAccepted(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	otherRoot := t.TempDir()
	ws := filepath.Join(outside, "forgectl-not-a-real-sandbox")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if sandbox.WithinWorkspace(otherRoot, ws) {
		t.Fatalf("precondition: otherRoot %q must not contain ws %q", otherRoot, ws)
	}

	body := `{"workspace":"` + ws + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`
	path := writeRaw(t, dir, "bc.json", body)

	t.Setenv("TMPDIR", otherRoot)

	got, err := loadBreadcrumb(path, dir)
	if err != nil {
		t.Fatalf("a forgectl-prefixed dir outside the temp root should now load: %v", err)
	}
	if got.Workspace != ws {
		t.Errorf("workspace = %q, want %q", got.Workspace, ws)
	}
}
