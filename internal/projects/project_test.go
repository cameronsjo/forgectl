package projects

import (
	"strings"
	"testing"
)

func TestParseRemoteURL(t *testing.T) {
	tests := []struct {
		name                      string
		url                       string
		wantHost, wantOwn, wantNm string
	}{
		{"github scp-like", "git@github.com:cameronsjo/forgectl.git", "github", "cameronsjo", "forgectl"},
		{"github https with .git", "https://github.com/cameronsjo/forgectl.git", "github", "cameronsjo", "forgectl"},
		{"github https no .git", "https://github.com/cameronsjo/forgectl", "github", "cameronsjo", "forgectl"},
		{"gitea ssh with port", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git", "gitea", "cameron", "homeclaw"},
		{"gitea ssh no port", "ssh://git@git.sjo.lol/cameron/homeclaw.git", "gitea", "cameron", "homeclaw"},
		{"gitea scp-like", "git@git.sjo.lol:cameron/homeclaw.git", "gitea", "cameron", "homeclaw"},
		{"unknown host falls through to bare hostname", "https://example.com/foo/bar.git", "example.com", "foo", "bar"},
		{"empty", "", "", "", ""},
		{"garbage", "not-a-url", "", "", ""},
		{"git:// scheme with creds — colon only before @, must not panic", "git://user:pass@github.com/owner/repo", "", "", ""},
		{"host only, no owner/name", "ssh://git@git.sjo.lol:222/", "gitea", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, owner, name := parseRemoteURL(tc.url)
			if host != tc.wantHost || owner != tc.wantOwn || name != tc.wantNm {
				t.Errorf("parseRemoteURL(%q) = (%q,%q,%q); want (%q,%q,%q)",
					tc.url, host, owner, name, tc.wantHost, tc.wantOwn, tc.wantNm)
			}
		})
	}
}

func TestParseCloneTarget(t *testing.T) {
	tests := []struct {
		name                          string
		arg                           string
		wantOK                        bool
		wantHost, wantOwner, wantName string
		wantSSHURL                    string
	}{
		{"github ssh URL", "git@github.com:cameronsjo/forgectl.git", true, "github", "cameronsjo", "forgectl", ""},
		{"github https URL", "https://github.com/cameronsjo/forgectl", true, "github", "cameronsjo", "forgectl", ""},
		{"bare owner/repo shorthand", "anthropics/claude-code", true, "github", "anthropics", "claude-code", ""},
		{"gitea ssh URL carries raw arg as SSHURL", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git", true,
			"gitea", "cameron", "homeclaw", "ssh://git@git.sjo.lol:222/cameron/homeclaw.git"},
		{"unrecognized host https carries raw arg as SSHURL", "https://example.com/foo/bar.git", true,
			"example.com", "foo", "bar", "https://example.com/foo/bar.git"},
		{"plain query, no slash", "forgectl", false, "", "", "", ""},
		{"too many slashes is not owner/repo shorthand", "a/b/c", false, "", "", "", ""},
		{"traversal segment rejected", "../etc", false, "", "", "", ""},
		{"empty", "", false, "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := ParseCloneTarget(tc.arg)
			if ok != tc.wantOK {
				t.Fatalf("ParseCloneTarget(%q) ok = %v; want %v", tc.arg, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if r.Host != tc.wantHost || r.Owner != tc.wantOwner || r.Name != tc.wantName || r.SSHURL != tc.wantSSHURL {
				t.Errorf("ParseCloneTarget(%q) = %+v; want host=%q owner=%q name=%q sshURL=%q",
					tc.arg, r, tc.wantHost, tc.wantOwner, tc.wantName, tc.wantSSHURL)
			}
		})
	}
}

func TestRepoKey(t *testing.T) {
	// Same name on two hosts must yield distinct keys (no bare-name collision).
	gh := Repo{Host: "github", Owner: "cameronsjo", Name: "homeclaw"}
	gt := Repo{Host: "gitea", Owner: "cameron", Name: "homeclaw"}
	if gh.Key() == gt.Key() {
		t.Errorf("cross-host repos share a key: %q", gh.Key())
	}

	// Case-insensitive.
	upper := Repo{Host: "GitHub", Owner: "CameronSjo", Name: "Forgectl"}
	lower := Repo{Host: "github", Owner: "cameronsjo", Name: "forgectl"}
	if upper.Key() != lower.Key() {
		t.Errorf("keys differ by case: %q vs %q", upper.Key(), lower.Key())
	}

	// Local-only repo (no parseable origin) keys by path.
	local := Repo{Name: "scratch", LocalPath: "/Users/x/Projects/scratch", Cloned: true}
	if got, want := local.Key(), "local:/Users/x/Projects/scratch"; got != want {
		t.Errorf("local Key() = %q; want %q", got, want)
	}
}

func TestRepoDisplayLine(t *testing.T) {
	tests := []struct {
		name string
		repo Repo
		want []string // substrings that must appear
	}{
		{"uncloned gitea", Repo{Host: "gitea", Owner: "cameron", Name: "homeclaw"}, []string{"git.sjo.lol", "homeclaw", "[uncloned]"}},
		{"cloned clean github", Repo{Host: "github", Owner: "cameronsjo", Name: "forgectl", Cloned: true}, []string{"gh", "forgectl", "[clean]"}},
		{"mirror flagged", Repo{Host: "gitea", Owner: "cameron", Name: "upstream", Mirror: true}, []string{"upstream (mirror)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.repo.DisplayLine()
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("DisplayLine() = %q; missing %q", got, sub)
				}
			}
		})
	}
}
