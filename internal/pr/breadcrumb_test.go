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
//   [x] CONTENT reject: workspace lacks the forgectl-workflow- sandbox prefix
//   [x] CONTENT reject: malformed ref string
//   [x] CONTENT reject: real on-disk bytes carrying a second document, a
//       truncated one, an embedded NUL, or any other trailing non-whitespace
//       (forgectl#289)
//   [x] CONTENT accept: the trailing whitespace writeBreadcrumb itself emits
//   [x] CONTENT reject: workspace is a symlink NAMED forgectl-workflow-*
//       pointing at an unprefixed victim dir (the resolve-then-Base pairing
//       is the sole identity gate after issue #184 — this is the case that
//       must never regress)
//   [x] CONTENT accept (pinned): workspace is a symlink whose own name lacks
//       the prefix but whose TARGET carries it — widened by issue #184,
//       benign only because callers act on the unresolved path
//   [x] CONTENT accept: workspace validates independent of the current
//       $TMPDIR (issue #184 — a workspace created under one $TMPDIR must
//       still validate after $TMPDIR changes; identity is the sandbox prefix
//       alone, not membership under the current OS temp root)
// FuzzLoadBreadcrumb
//   [x] Every returned workspace exists, is a directory, resolves, and has the
//       sandbox prefix on its resolved base name
// writeBreadcrumb round-trips through loadBreadcrumb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeWorkspace makes a dir under the OS temp root with the forgectl-workflow-
// prefix — what validateWorkspace accepts as a real sandbox.
func fakeWorkspace(t *testing.T) string {
	t.Helper()
	ws, err := os.MkdirTemp("", "forgectl-workflow-test-*")
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
		// Forged locality: the flag claims a local session while the ref names a
		// real-looking owner. Only PrepareLocal writes local:true, and it always
		// stamps owner "local", so the two can never legitimately disagree.
		{"local flag with non-local owner", `{"workspace":"` + ws + `","ref":"cameronsjo/forgectl#1","agent":"a","createdAt":"2026-07-08T00:00:00Z","local":true}`},
		// A bare number parses but leaves Owner/Repo empty — Slug() would be "/".
		{"incomplete bare-number ref", `{"workspace":"` + ws + `","ref":"5","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`},
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

// TestLoadBreadcrumb_RejectsTrailingContentOnDisk drives forgectl#289 through
// the real path — bytes written to a real file in a real session-state dir and
// read back by the loader — rather than handing strings straight to the
// decoder. The tails are the shapes a file actually takes when something else
// has written to it: a second record, a partially written one, and NUL padding.
func TestLoadBreadcrumb_RejectsTrailingContentOnDisk(t *testing.T) {
	dir := t.TempDir()
	ws := fakeWorkspace(t)
	record := `{"workspace":"` + ws + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`
	second := `{"workspace":"` + ws + `","ref":"o/r#2","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`

	cases := []struct {
		name string
		body string
	}{
		{"second document", record + second},
		{"second document after newline", record + "\n" + second},
		{"second document after whitespace", record + "  \t\r\n" + second},
		{"truncated second document", record + "\n" + `{"workspace":"/tmp/forgectl-workflow-`},
		{"embedded NUL then document", record + "\x00" + second},
		{"NUL padding", record + "\n\x00\x00\x00"},
		{"trailing garbage", record + "\nlolwat"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRaw(t, dir, "trail"+string(rune('a'+i))+".json", tc.body)
			if _, err := loadBreadcrumb(path, dir); err == nil {
				t.Errorf("%s: loaded a breadcrumb file carrying trailing content", tc.name)
			}
		})
	}
}

// TestLoadBreadcrumb_AcceptsWriterTrailingWhitespace is the companion accept
// case, and it is what proves #289 is a hardening and not a migration:
// writeBreadcrumb terminates every file with "\n", so the tightening has to
// leave whitespace tails alone.
func TestLoadBreadcrumb_AcceptsWriterTrailingWhitespace(t *testing.T) {
	dir := t.TempDir()
	ws := fakeWorkspace(t)
	record := `{"workspace":"` + ws + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`

	for i, tail := range []string{"", "\n", "\r\n", "\n\n  \t\n"} {
		path := writeRaw(t, dir, "ws"+string(rune('a'+i))+".json", record+tail)
		got, err := loadBreadcrumb(path, dir)
		if err != nil {
			t.Fatalf("trailing whitespace %q must still load: %v", tail, err)
		}
		if got.Ref != "o/r#1" {
			t.Errorf("ref = %q, want %q", got.Ref, "o/r#1")
		}
	}
}

// TestLoadBreadcrumb_LocalityCheckIsOneDirectional pins the other half of the
// cross-representation check. Rejecting a forged local flag must not also
// reject the two shapes that are legitimate: a genuine local session (flag +
// owner "local"), and an owner literally named "local" with no flag — which is
// both a real forge repo (git.sjo.lol/local/tools, issue #185) and the shape
// every pre-upgrade local breadcrumb takes.
func TestLoadBreadcrumb_LocalityCheckIsOneDirectional(t *testing.T) {
	dir := t.TempDir()
	ws := fakeWorkspace(t)

	cases := []struct {
		name      string
		body      string
		wantLocal bool
	}{
		{
			"genuine local session",
			`{"workspace":"` + ws + `","ref":"local/abc1234#1","agent":"a","createdAt":"2026-07-08T00:00:00Z","local":true}`,
			true,
		},
		{
			"real forge owner named local, no flag",
			`{"workspace":"` + ws + `","ref":"local/tools#5","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`,
			false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRaw(t, dir, "ok"+string(rune('a'+i))+".json", tc.body)
			bc, err := loadBreadcrumb(path, dir)
			if err != nil {
				t.Fatalf("%s must load: %v", tc.name, err)
			}
			if bc.Local != tc.wantLocal {
				t.Errorf("Local = %v, want %v", bc.Local, tc.wantLocal)
			}
		})
	}
}

// FuzzLoadBreadcrumb mutates breadcrumb JSON against a fixed session-state dir.
// The security invariant: any breadcrumb the loader RETURNS (no error) must
// point at a Workspace that exists, is a directory, resolves successfully,
// and carries the forgectl-workflow- sandbox prefix on its resolved base name
// — the loader never yields a breadcrumb steering a later `git -C` at an
// arbitrary path. It deliberately does NOT assert Workspace sits under the
// current OS temp root: validateWorkspace no longer checks that (issue #184 —
// that check was $TMPDIR-dependent and made `pr list`/`attach`/`teardown` go
// blind to a pre-existing session the moment $TMPDIR changed), so a workspace
// outside the CURRENT temp root is a valid pass here, not a bug. Seeded with
// valid breadcrumbs pointing at a real sandbox dir.
func FuzzLoadBreadcrumb(f *testing.F) {
	sessionsDir := f.TempDir()
	ws, err := os.MkdirTemp("", "forgectl-workflow-fuzz-*")
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
		info, err := os.Stat(bc.Workspace)
		if err != nil || !info.IsDir() {
			t.Errorf("loaded breadcrumb workspace %q is not an existing directory: %v", bc.Workspace, err)
		}
		resolved, err := filepath.EvalSymlinks(bc.Workspace)
		if err != nil {
			t.Fatalf("loaded breadcrumb workspace %q is not resolvable: %v", bc.Workspace, err)
		}
		if !strings.HasPrefix(filepath.Base(resolved), sandboxPrefix) {
			t.Errorf("loaded breadcrumb workspace %q resolves without the %q sandbox prefix", bc.Workspace, sandboxPrefix)
		}
	})
}

// TestLoadBreadcrumb_TMPDIRIndependent regresses issue #184: a workspace
// created under one $TMPDIR must still validate after $TMPDIR changes.
// os.TempDir() reads $TMPDIR at CALL time, so gating validateWorkspace on the
// current temp root made every pre-existing session invisible to
// `pr list`/`attach`/`teardown`/`cleanup` the instant $TMPDIR changed — RED
// before this fix, since validateWorkspace used to compare workspace against
// osTempDir() at validation time.
func TestLoadBreadcrumb_TMPDIRIndependent(t *testing.T) {
	dir := t.TempDir()
	ws := fakeWorkspace(t) // created under the current $TMPDIR
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	bc := Breadcrumb{Workspace: ws, Ref: ref.String(), Agent: "claude", CreatedAt: time.Now().UTC()}
	path, err := writeBreadcrumb(dir, ref, bc)
	if err != nil {
		t.Fatalf("writeBreadcrumb: %v", err)
	}

	// Point $TMPDIR somewhere the workspace is NOT under.
	t.Setenv("TMPDIR", t.TempDir())

	got, err := loadBreadcrumb(path, dir)
	if err != nil {
		t.Fatalf("loadBreadcrumb: %v (workspace validation must not depend on the current $TMPDIR)", err)
	}
	if got.Workspace != ws {
		t.Errorf("got.Workspace = %q, want %q", got.Workspace, ws)
	}
}

// TestLoadBreadcrumb_WorkspaceSymlinkNamePrefixedTargetNot is the case that
// must never regress. A symlink NAMED forgectl-workflow-* pointing at an
// unprefixed victim directory must be REJECTED.
//
// After issue #184 retired the temp-root bound, the EvalSymlinks +
// filepath.Base + HasPrefix triple in validateWorkspace is the ONLY control
// between a breadcrumb and os.RemoveAll. Nothing else reaches it:
// TestLoadBreadcrumb_SymlinkEscape covers the LOCATION guard (a symlinked
// breadcrumb PATH), TestLoadBreadcrumb_WorkspaceBadPrefix uses a plain
// directory with no link, and FuzzLoadBreadcrumb only mutates JSON — a fuzzer
// never synthesises a symlink whose name and target disagree. Reorder or drop
// the resolve-then-Base pairing (prefix-check the unresolved path instead) and
// this breadcrumb becomes a deletion of an arbitrary directory.
func TestLoadBreadcrumb_WorkspaceSymlinkNamePrefixedTargetNot(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()

	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	link := filepath.Join(root, "forgectl-workflow-decoy")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	body := `{"workspace":"` + link + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`
	path := writeRaw(t, dir, "bc.json", body)
	if _, err := loadBreadcrumb(path, dir); err == nil {
		t.Error("expected rejection: a symlink named forgectl-workflow-* must not launder an unprefixed target past validateWorkspace")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim dir must be untouched by validation: %v", err)
	}
}

// TestLoadBreadcrumb_WorkspaceSymlinkTargetPrefixed pins the OTHER direction,
// which issue #184 widened: a symlink whose own name lacks the prefix but
// whose target carries it is ACCEPTED, because validateWorkspace checks the
// RESOLVED path.
//
// This documents accepted behaviour, not a desired guarantee. It is benign
// only because sandbox.Teardown acts on the UNRESOLVED string, so os.RemoveAll
// unlinks the link and leaves the target alone (both ends carry a comment
// saying so). Pinning it means a later change cannot move this case silently
// in either direction — tightening it to a rejection is a behaviour change,
// and loosening Teardown to act on the resolved path makes it destructive.
func TestLoadBreadcrumb_WorkspaceSymlinkTargetPrefixed(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()

	target := filepath.Join(root, "forgectl-workflow-real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(root, "plain-name")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	body := `{"workspace":"` + link + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`
	path := writeRaw(t, dir, "bc.json", body)
	got, err := loadBreadcrumb(path, dir)
	if err != nil {
		t.Fatalf("loadBreadcrumb: %v (a link resolving to a prefixed sandbox is accepted today)", err)
	}
	// The UNRESOLVED string is what comes back and what callers act on.
	if got.Workspace != link {
		t.Errorf("got.Workspace = %q, want the unresolved link path %q", got.Workspace, link)
	}
}

func TestLoadBreadcrumb_WorkspaceBadPrefix(t *testing.T) {
	dir := t.TempDir()
	// A real, existing dir (t.TempDir is under the OS temp root) that lacks the
	// forgectl-workflow- sandbox prefix — so the prefix branch of
	// validateWorkspace must reject it even though it exists and is a directory.
	notASandbox := t.TempDir()
	body := `{"workspace":"` + notASandbox + `","ref":"o/r#1","agent":"a","createdAt":"2026-07-08T00:00:00Z"}`
	path := writeRaw(t, dir, "bc.json", body)
	if _, err := loadBreadcrumb(path, dir); err == nil {
		t.Error("expected rejection for a workspace lacking the forgectl-workflow- sandbox prefix")
	}
}
