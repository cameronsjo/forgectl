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
//
// slugifyRepo (Classification: pure logic / sanitizer)
//   [x] Happy: a directory name with a space and mixed case slugifies to a
//       valid docker repository component
//   [x] Happy: a lone '_' and a doubled '__' survive as themselves (both
//       are separator tokens in the repository grammar)
//   [x] Boundary: a LEADING '_' is dropped, not preserved — "_scratch",
//       "_build", "_site" are ordinary directory names (Hugo, Sphinx) and
//       the repository grammar requires a leading [a-z0-9]
//   [x] Boundary: a TRAILING '_' is dropped for the same reason
//   [x] Boundary: an underscore adjacent to a collapsed separator
//       ("app_ backup") collapses to one '-' — "_-" is not a separator
//       token even though it is a valid tag
//   [x] Boundary: an underscore run longer than '__' collapses to '-'
//   [x] Boundary: a leading digit is valid and passes through
//   [x] Boundary: a name that is entirely underscores falls back to "image"
//   [x] Boundary: an empty or fully-invalid name falls back to "image"
//       (not slugifyBranch's "branch" — the two sanitizers label different
//       kinds of absence, and since forgectl#187's review they no longer
//       share an implementation at all)
//   [x] Property: every slugifyRepo output parses as a docker repository
//       name (dockerRepoName below)
//
// The tag and repository grammars diverge, so slugifyBranch keeps the
// permissive tag sanitizer and slugifyRepo gets its own — deliberately not
// tightening the branch path, which is not what broke.

import (
	"regexp"
	"strings"
	"testing"
)

// dockerRepoName is the docker/distribution reference grammar for a single
// repository *name component*, anchored. Stricter than the tag grammar: it
// must start and end with an alphanumeric, and each separator must be a
// lone '.', a lone '_', a doubled '__', or a run of '-' — never a mix.
//
// Verified against the real parser rather than a reading of it: on the host
// (docker 29.6.2) `docker image inspect <name>:dev` answers "invalid
// reference format" for a name this regex rejects and "No such image" for
// one it accepts — no image built, no registry contacted. Probed both ways
// for _scratch, app_-backup, my___project, trailing_ (all rejected) and
// scratch, app-backup, my__project, my_project, 9lives, hidden-dir, image
// (all accepted).
var dockerRepoName = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)

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

func TestSlugifyRepo(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		want string
	}{
		{"already valid, lowercased", "MyProject", "myproject"},
		{"space collapses to dash", "My Project", "my-project"},
		{"runs of invalid chars collapse to one dash", "my!!!project", "my-project"},
		{"leading/trailing separators trimmed", ".hidden-dir.", "hidden-dir"},
		{"empty falls back to image", "", "image"},
		{"fully invalid falls back to image", "///!!!", "image"},
		{"lone underscore is a separator token", "my_project", "my_project"},
		{"doubled underscore is a separator token", "my__project", "my__project"},
		{"longer underscore run collapses to dash", "my___project", "my-project"},
		{"leading underscore dropped (hugo scratch dir)", "_scratch", "scratch"},
		{"leading underscore dropped (generated build dir)", "_build", "build"},
		{"leading underscore dropped (sphinx site dir)", "_site", "site"},
		{"trailing underscore dropped", "trailing_", "trailing"},
		{"underscore adjacent to a collapsed separator", "app_ backup", "app-backup"},
		{"all underscores falls back to image", "____", "image"},
		{"leading digit passes through", "9lives", "9lives"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slugifyRepo(tc.dir)
			if got != tc.want {
				t.Errorf("slugifyRepo(%q) = %q, want %q", tc.dir, got, tc.want)
			}
			if !dockerRepoName.MatchString(got) {
				t.Errorf("slugifyRepo(%q) = %q, which docker rejects as a repository name", tc.dir, got)
			}
		})
	}
}

// TestSlugifyRepo_TruncationLeavesAValidName guards the one place the
// separator-run logic can't reach: maxTagLen truncation, which can land
// mid-separator and leave a dangling '_', '.', or '-'.
func TestSlugifyRepo_TruncationLeavesAValidName(t *testing.T) {
	// Lands the cut squarely on the separator: maxTagLen-1 alphanumerics,
	// then "__", then more.
	long := strings.Repeat("a", maxTagLen-1) + "__" + strings.Repeat("b", 40)
	got := slugifyRepo(long)
	if len(got) > maxTagLen {
		t.Errorf("slugifyRepo result length = %d, want <= %d", len(got), maxTagLen)
	}
	if !dockerRepoName.MatchString(got) {
		t.Errorf("slugifyRepo truncation produced %q, which docker rejects as a repository name", got)
	}
}
