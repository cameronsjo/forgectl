package docker

// Test plan for tag.go
//
// deriveTag / devTag (Classification: pure logic / data transformer)
//   [x] Happy: deriveTag joins repo, slugified branch, and shortsha as
//       "{repo}:{branch-slug}-{shortsha}"
//   [x] Happy: devTag appends the fixed ":dev" alias to repo
//
// slugifyBranch (Classification: pure logic / sanitizer)
//   [x] Happy: already-valid branch names pass through lowercased
//   [x] Happy: '/' (and runs of invalid characters) collapse to a single '-'
//   [x] Happy: '_' is preserved (a valid docker tag character)
//   [x] Boundary: leading/trailing separators are trimmed
//   [x] Boundary: an empty or fully-invalid branch falls back to "branch"
//   [x] Boundary: a branch beginning with '-' (git-argv-injection shaped)
//       never survives as a leading '-' in the slug
//   [x] Boundary: a branch longer than 128 chars is truncated to maxTagLen

import (
	"strings"
	"testing"
)

func TestDeriveTag(t *testing.T) {
	got := deriveTag("myrepo", "Feature/Foo", "abc1234")
	want := "myrepo:feature-foo-abc1234"
	if got != want {
		t.Errorf("deriveTag = %q, want %q", got, want)
	}
}

func TestDevTag(t *testing.T) {
	if got := devTag("myrepo"); got != "myrepo:dev" {
		t.Errorf("devTag = %q, want %q", got, "myrepo:dev")
	}
}

func TestSlugifyBranch(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   string
	}{
		{"already valid, lowercased", "Main", "main"},
		{"slash collapses to dash", "feature/foo-bar", "feature-foo-bar"},
		{"underscore preserved", "release_2026", "release_2026"},
		{"runs of invalid chars collapse to one dash", "foo!!!bar", "foo-bar"},
		{"leading/trailing separators trimmed", "/weird-branch-/", "weird-branch"},
		{"empty falls back to branch", "", "branch"},
		{"fully invalid falls back to branch", "///!!!", "branch"},
		{"leading dash never survives", "-x", "x"},
		{"leading dash injection shape", "--upload-pack=x", "upload-pack-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slugifyBranch(tc.branch)
			if got != tc.want {
				t.Errorf("slugifyBranch(%q) = %q, want %q", tc.branch, got, tc.want)
			}
			if strings.HasPrefix(got, "-") {
				t.Errorf("slugifyBranch(%q) = %q must never start with '-'", tc.branch, got)
			}
		})
	}
}

func TestSlugifyBranch_TruncatesToMaxTagLen(t *testing.T) {
	long := strings.Repeat("a", maxTagLen+50)
	got := slugifyBranch(long)
	if len(got) > maxTagLen {
		t.Errorf("slugifyBranch result length = %d, want <= %d", len(got), maxTagLen)
	}
	if got != strings.Repeat("a", maxTagLen) {
		t.Errorf("slugifyBranch truncation produced %q, want %d 'a's", got, maxTagLen)
	}
}
