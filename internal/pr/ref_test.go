package pr

// Test plan for ref.go
//
// ParseRef (Classification: hostile-input validation)
//   [x] Accept: owner/repo#N
//   [x] Accept: full github.com PR URL (with and without trailing slash)
//   [x] Accept: bare N → Ref{Number:N}, Owner/Repo empty (incomplete)
//   [x] Accept: owner/repo charset edge (dots, underscores, hyphens)
//   [x] Reject: foo#bar (non-numeric N)
//   [x] Reject: leading '-' (option-like)
//   [x] Reject: ../../etc path traversal
//   [x] Reject: shell metacharacters (a/b#1;rm -rf)
//   [x] Reject: URL with extra path segments
//   [x] Reject: oversized N (integer overflow)
//   [x] Reject: unicode trickery / whitespace-embedded
//   [x] Reject: zero / empty
// ResolveRef (Classification: ops layer, Runner-backed origin resolution)
//   [x] Complete ref passes through without a Runner call
//   [x] Bare N resolves owner/repo from `gh repo view`
//   [x] Bare N falls back to `git remote get-url origin` when gh fails
//   [x] Origin owner/repo outside the charset is rejected
//   [x] A typed ref with owner "local" is refused (reserved sentinel)
//   [x] A resolved origin owner "local" is refused (reserved sentinel)

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestParseRef_Accept(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Ref
	}{
		{"slug", "cameronsjo/forgectl#42", Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}},
		{"url", "https://github.com/cameronsjo/forgectl/pull/7", Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 7}},
		{"url trailing slash", "https://github.com/cameronsjo/forgectl/pull/7/", Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 7}},
		{"bare", "42", Ref{Number: 42}},
		{"charset", "a.b_c-d/e.f_g-h#1", Ref{Owner: "a.b_c-d", Repo: "e.f_g-h", Number: 1}},
		{"trim whitespace", "  cameronsjo/forgectl#42  ", Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRef(tc.in)
			if err != nil {
				t.Fatalf("ParseRef(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRef(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRef_Reject(t *testing.T) {
	rejects := []struct {
		name string
		in   string
	}{
		{"non-numeric N", "foo#bar"},
		{"leading dash", "-flag"},
		{"leading dash slug owner", "-owner/repo#1"},
		{"path traversal", "../../etc"},
		{"traversal slug", "../a/b#1"},
		{"shell metachars", "a/b#1;rm -rf"},
		{"space injection", "a/b#1 --upload-pack=x"},
		{"url extra segments", "https://github.com/o/r/pull/1/extra"},
		{"url wrong path", "https://github.com/o/r/issues/1"},
		{"url non-github host", "https://evil.com/o/r/pull/1"},
		{"oversized N", "999999999999999999999999999999"},
		{"zero", "0"},
		{"empty", ""},
		{"blank", "   "},
		{"unicode digit", "４2"},
		{"newline embedded", "a/b#1\n"},
		{"three segments slug", "a/b/c#1"},
		{"missing number", "owner/repo#"},
		{"hash only", "#1"},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseRef(tc.in); err == nil {
				t.Errorf("ParseRef(%q) = %+v, want error", tc.in, got)
			}
		})
	}
}

// FuzzParseRef exercises the pure ParseRef validator. On ANY non-error return
// the number must be positive; and for a complete ref (slug/URL form) the
// owner and repo must satisfy ValidOwnerRepoPart — the same anchored guard the
// argv paths rely on — and the ref must round-trip through String()/ParseRef.
// A bare-number ref (empty Owner/Repo) does not stringify to a re-parseable
// form, so the round-trip and charset assertions apply only to complete refs.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{
		"cameronsjo/forgectl#42",
		"https://github.com/cameronsjo/forgectl/pull/7",
		"42",
		"a.b_c-d/e.f_g-h#1",
		"-flag",
		"../../etc",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseRef(s)
		if err != nil {
			return
		}
		if ref.Number <= 0 {
			t.Errorf("ParseRef(%q) accepted a non-positive number: %+v", s, ref)
		}
		if !ref.Complete() {
			return // bare-number form: Owner/Repo empty by design
		}
		if !ValidOwnerRepoPart(ref.Owner) {
			t.Errorf("ParseRef(%q) owner %q fails ValidOwnerRepoPart", s, ref.Owner)
		}
		if !ValidOwnerRepoPart(ref.Repo) {
			t.Errorf("ParseRef(%q) repo %q fails ValidOwnerRepoPart", s, ref.Repo)
		}
		rt, err := ParseRef(ref.String())
		if err != nil {
			t.Errorf("ParseRef(%q).String()=%q failed to re-parse: %v", s, ref.String(), err)
		} else if rt != ref {
			t.Errorf("round-trip mismatch: %+v -> %q -> %+v", ref, ref.String(), rt)
		}
	})
}

func TestResolveRef_CompletePassthrough(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake)
	got, err := c.ResolveRef(context.Background(), "cameronsjo/forgectl#42")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != (Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}) {
		t.Errorf("ResolveRef = %+v", got)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("complete ref should not shell out; got calls %+v", fake.Calls)
	}
}

func TestResolveRef_BareViaGh(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "gh" {
				return "cameronsjo/forgectl", nil
			}
			return "", errors.New("unexpected call")
		},
	}
	c := New(fake)
	got, err := c.ResolveRef(context.Background(), "42")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	want := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	if got != want {
		t.Errorf("ResolveRef = %+v, want %+v", got, want)
	}
}

func TestResolveRef_BareViaGitFallback(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "gh" {
				return "", errors.New("gh not authed")
			}
			if name == "git" {
				return "git@github.com:cameronsjo/forgectl.git", nil
			}
			return "", errors.New("unexpected call")
		},
	}
	c := New(fake)
	got, err := c.ResolveRef(context.Background(), "42")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	want := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	if got != want {
		t.Errorf("ResolveRef = %+v, want %+v", got, want)
	}
}

func TestResolveRef_BadOriginRejected(t *testing.T) {
	// Each case is what resolveOrigin returns for a bare-number resolve. The
	// origin is hostile input, so the resolved owner/repo must pass the SAME
	// bundled guard (ValidOwnerRepoPart) the typed-ref path uses — anchored
	// charset PLUS the leading-'-' and ".." rejections, not the bare charset
	// regex alone (which admits both).
	cases := []struct {
		name   string
		origin string // as returned by `gh repo view` (slug form)
	}{
		{"shell metachars", "owner/repo;rm -rf"},
		{"leading dash owner", "-x/repo"}, // via a git@github.com:-x/repo.git origin
		{"leading dash repo", "owner/-x"}, // symmetric: a repo component
		{"dotdot owner", "../repo"},       // via a https://github.com/../repo.git origin
		{"dotdot repo", "owner/.."},       // symmetric
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{
				RunFunc: func(name string, args []string) (string, error) {
					if name == "gh" {
						return tc.origin, nil
					}
					return "", errors.New("unexpected")
				},
			}
			c := New(fake)
			if got, err := c.ResolveRef(context.Background(), "42"); err == nil {
				t.Errorf("ResolveRef with hostile origin %q = %+v, want error", tc.origin, got)
			}
		})
	}
}

// TestResolveRef_LocalOwnerReserved verifies the windowName/PostReview
// disambiguation guarantee holds: a real PR reference can never resolve to
// owner "local", since localRef's synthetic Refs key off exactly that value.
func TestResolveRef_LocalOwnerReserved(t *testing.T) {
	t.Run("typed directly", func(t *testing.T) {
		fake := &exec.FakeRunner{}
		c := New(fake)
		if got, err := c.ResolveRef(context.Background(), "local/repo#5"); err == nil {
			t.Errorf("ResolveRef(\"local/repo#5\") = %+v, want error (reserved owner)", got)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("a rejected owner should not shell out; got %+v", fake.Calls)
		}
	})
	t.Run("resolved from origin", func(t *testing.T) {
		fake := &exec.FakeRunner{
			RunFunc: func(name string, args []string) (string, error) {
				if name == "gh" {
					return "local/somerepo", nil
				}
				return "", errors.New("unexpected call")
			},
		}
		c := New(fake)
		if got, err := c.ResolveRef(context.Background(), "42"); err == nil {
			t.Errorf("ResolveRef with origin owner \"local\" = %+v, want error", got)
		}
	})
}

// TestParseRef_LocalOwnerStillPermitted verifies ParseRef itself stays
// permissive for owner "local" — the reservation is enforced only in
// ResolveRef. This is required for breadcrumb reload: validateBreadcrumb
// calls ParseRef directly on a synthetic local Ref's String(), which contains
// owner "local".
func TestParseRef_LocalOwnerStillPermitted(t *testing.T) {
	got, err := ParseRef("local/abc1234#1")
	if err != nil {
		t.Fatalf("ParseRef(\"local/abc1234#1\") unexpected error: %v", err)
	}
	want := Ref{Owner: "local", Repo: "abc1234", Number: 1}
	if got != want {
		t.Errorf("ParseRef(\"local/abc1234#1\") = %+v, want %+v", got, want)
	}
}
