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
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "gh" {
				return "owner/repo;rm -rf", nil
			}
			return "", errors.New("unexpected")
		},
	}
	c := New(fake)
	if got, err := c.ResolveRef(context.Background(), "42"); err == nil {
		t.Errorf("ResolveRef with hostile origin = %+v, want error", got)
	}
}
