package pr

// Test plan for member.go (#212)
//
// resolveBreadcrumbMember (Classification: unlink authority, fail-closed)
//   [x] An exact member resolves to itself
//   [x] A "./" alias and a parent-relative alias resolve to the real member
//   [x] An OUTSIDE symlink pointing into the dir resolves to the real member —
//       and the returned path is the real member, never the link
//   [x] An exact in-directory symlink selects its REAL target member
//   [x] An in-directory symlink whose target is not an enumerated member rejects
//   [x] A non-member, a glob, and a directory reject
//   [x] Two names resolving to one target reject an ambiguous operand, while an
//       exact lexical name still selects deterministically
//   [x] The captured bytes and record match the file on disk
//   [x] A hostile enumerated filename yields one terminal-safe display path
// Alias unlink (Classification: deletes the real record, never the alias)
//   [x] Teardown through every alias form removes the REAL breadcrumb
//   [x] Every rejection issues ZERO Runner calls and mutates nothing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// testClientAt builds a client over an EXISTING session-state dir, so a
// rejection case can use a fresh Runner ledger while sharing the seeded
// directory under test.
func testClientAt(t *testing.T, fake *exec.FakeRunner, sessionsDir string) *Client {
	t.Helper()
	return New(fake,
		WithSessionsDir(sessionsDir),
		WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }),
	)
}

// canaries records the state that must survive any teardown, successful or
// refused: an unrelated sibling breadcrumb, the session directory itself, and
// a file outside it entirely.
type canaries struct {
	sibling  string
	external string
	dir      string
}

func seedCanaries(t *testing.T, c *Client) canaries {
	t.Helper()
	sibling, _ := seedSession(t, c, Ref{Owner: "other", Repo: "repo", Number: 99}, time.Now().UTC())
	external := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(external, []byte("survives"), 0o600); err != nil {
		t.Fatalf("seed external canary: %v", err)
	}
	return canaries{sibling: sibling, external: external, dir: c.SessionsDir()}
}

func (cn canaries) assertIntact(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(cn.sibling); err != nil {
		t.Errorf("sibling breadcrumb must be untouched: %v", err)
	}
	if body, err := os.ReadFile(cn.external); err != nil || string(body) != "survives" {
		t.Errorf("external canary changed: body=%q err=%v", body, err)
	}
	info, err := os.Stat(cn.dir)
	if err != nil || !info.IsDir() {
		t.Errorf("session dir must survive: info=%v err=%v", info, err)
	}
}

// TestResolveBreadcrumbMember_AliasFormsSelectTheRealMember runs each alias
// form against its OWN seeded directory. Staging several aliases at once would
// make the target legitimately reachable under multiple names, which the
// ambiguity rule refuses — that shape is covered separately.
func TestResolveBreadcrumbMember_AliasFormsSelectTheRealMember(t *testing.T) {
	// Each case receives the real member path and returns the operand to test.
	cases := map[string]func(t *testing.T, c *Client, real string) string{
		"exact member": func(_ *testing.T, _ *Client, real string) string { return real },
		"dot alias": func(_ *testing.T, _ *Client, real string) string {
			return filepath.Join(filepath.Dir(real), ".", filepath.Base(real))
		},
		"parent alias": func(_ *testing.T, _ *Client, real string) string {
			return filepath.Join(filepath.Dir(real), "sub", "..", filepath.Base(real))
		},
		"outside symlink": func(t *testing.T, _ *Client, real string) string {
			link := filepath.Join(t.TempDir(), "outside-link.json")
			if err := os.Symlink(real, link); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			return link
		},
		"in-dir symlink": func(t *testing.T, c *Client, real string) string {
			link := filepath.Join(c.SessionsDir(), "in-dir-link.json")
			if err := os.Symlink(real, link); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			return link
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, &exec.FakeRunner{})
			real, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())
			operand := build(t, c, real)

			member, err := c.resolveBreadcrumbMember(operand)
			if err != nil {
				t.Fatalf("resolveBreadcrumbMember(%q): %v", operand, err)
			}
			if member.path != real {
				t.Errorf("authoritative path = %q, want the REAL member %q — never the alias",
					member.path, real)
			}
			onDisk, err := os.ReadFile(real)
			if err != nil {
				t.Fatalf("read real member: %v", err)
			}
			if string(member.bytes) != string(onDisk) {
				t.Error("captured bytes must equal the real member's contents")
			}
			if member.breadcrumb.Ref != "o/r#1" {
				t.Errorf("captured record ref = %q, want %q", member.breadcrumb.Ref, "o/r#1")
			}
		})
	}
}

func TestResolveBreadcrumbMember_CapturesTerminalSafeDisplayPath(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	realPath, workspace := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())
	hostile := filepath.Join(c.SessionsDir(), "planted-\x1b[2K\rinnocent\u202egnj.json")
	if err := os.Rename(realPath, hostile); err != nil {
		t.Fatalf("rename breadcrumb to hostile filename: %v", err)
	}

	member, err := c.resolveBreadcrumbMember(hostile)
	if err != nil {
		t.Fatalf("resolve hostile breadcrumb: %v", err)
	}
	if member.displayPath == member.path {
		t.Fatalf("display path was not escaped: %q", member.displayPath)
	}

	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("recreate workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	if err := c.discardStale(member); err == nil {
		t.Fatal("discardStale must refuse after the workspace reappears")
	} else {
		assertTerminalSafeBreadcrumbError(t, err)
	}
}

// TestTeardown_ThroughAliasRemovesTheRealBreadcrumb is the consequence that
// matters: an alias must not let teardown report success while the real
// breadcrumb survives (or while only the alias is unlinked).
func TestTeardown_ThroughAliasRemovesTheRealBreadcrumb(t *testing.T) {
	for _, name := range []string{"outside symlink", "in-dir symlink"} {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			c := testClient(t, fake)
			cn := seedCanaries(t, c)
			real, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())

			var operand string
			if name == "outside symlink" {
				operand = filepath.Join(t.TempDir(), "outside-link.json")
			} else {
				operand = filepath.Join(c.SessionsDir(), "in-dir-link.json")
			}
			if err := os.Symlink(real, operand); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}

			if err := c.Teardown(context.Background(), operand); err != nil {
				t.Fatalf("Teardown via %s: %v", name, err)
			}
			if _, err := os.Lstat(real); !os.IsNotExist(err) {
				t.Errorf("the REAL breadcrumb must be removed, not merely the alias; Lstat err = %v", err)
			}
			if len(fake.Calls) != 0 {
				t.Errorf("a stale teardown must issue ZERO Runner calls; got %+v", fake.Calls)
			}
			cn.assertIntact(t)
		})
	}
}

func TestResolveBreadcrumbMember_Rejections(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	real, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())

	// A link INSIDE the dir pointing at a .json that is not itself an
	// enumerated member — the shape that would otherwise reach past the guard.
	outsideTarget := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outsideTarget, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seed outside target: %v", err)
	}
	escapingLink := filepath.Join(c.SessionsDir(), "escaping.json")
	if err := os.Symlink(outsideTarget, escapingLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	nonMember := filepath.Join(t.TempDir(), "attacker.json")
	if err := os.WriteFile(nonMember, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("seed non-member: %v", err)
	}
	subdir := filepath.Join(c.SessionsDir(), "nested.json")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("seed directory named .json: %v", err)
	}

	cases := map[string]string{
		"non-member":             nonMember,
		"glob":                   filepath.Join(c.SessionsDir(), "*.json"),
		"directory named .json":  subdir,
		"link escaping the set":  escapingLink,
		"absent path in the dir": filepath.Join(c.SessionsDir(), "no-such.json"),
		"the session dir itself": c.SessionsDir(),
		"empty operand":          "",
	}
	for name, operand := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			fc := testClientAt(t, fake, c.SessionsDir())
			if _, err := fc.resolveBreadcrumbMember(operand); err == nil {
				t.Fatalf("resolveBreadcrumbMember(%q) accepted a non-member", operand)
			}
			if err := fc.Teardown(context.Background(), operand); err == nil {
				t.Errorf("Teardown(%q) must refuse", operand)
			}
			if len(fake.Calls) != 0 {
				t.Errorf("a refusal must issue ZERO Runner calls; got %+v", fake.Calls)
			}
			if _, err := os.Stat(real); err != nil {
				t.Errorf("a refusal must not remove the real breadcrumb: %v", err)
			}
			if _, err := os.Stat(outsideTarget); err != nil {
				t.Errorf("a refusal must not remove a link's outside target: %v", err)
			}
		})
	}
}

// TestResolveBreadcrumbMember_AmbiguityNeedsAnExactName pins the disambiguation
// rule: when two names in the directory resolve to one file, an operand that
// is not an exact lexical member cannot say which was meant, so it refuses —
// while naming either exact file still works deterministically.
func TestResolveBreadcrumbMember_AmbiguityNeedsAnExactName(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	real, _ := seedStaleSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, time.Now().UTC())
	inDirLink := filepath.Join(c.SessionsDir(), "alias.json")
	if err := os.Symlink(real, inDirLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// An OUTSIDE link resolves to the same target as both in-dir names.
	outside := filepath.Join(t.TempDir(), "ambiguous.json")
	if err := os.Symlink(real, outside); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := c.resolveBreadcrumbMember(outside); err == nil {
		t.Error("an operand resolving to a target reachable under two names must refuse")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("an ambiguous refusal must issue ZERO Runner calls; got %+v", fake.Calls)
	}
	if _, err := os.Stat(real); err != nil {
		t.Errorf("an ambiguous refusal must mutate nothing: %v", err)
	}

	// Both exact names select the same authoritative real member.
	for _, operand := range []string{real, inDirLink} {
		member, err := c.resolveBreadcrumbMember(operand)
		if err != nil {
			t.Fatalf("exact name %q must resolve: %v", operand, err)
		}
		if member.path != real {
			t.Errorf("exact name %q selected %q, want the real member %q", operand, member.path, real)
		}
	}
}
